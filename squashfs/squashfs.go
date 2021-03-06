// This package is a small go "library" (read: exec wrapper) around the
// mksquashfs binary that provides some useful primitives.
package squashfs

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"

	stackermtree "github.com/anuvu/stacker/mtree"
	stackeroci "github.com/anuvu/stacker/oci"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci"
	"github.com/opencontainers/umoci/oci/casext"
	"github.com/opencontainers/umoci/pkg/fseval"
	"github.com/opencontainers/umoci/pkg/mtreefilter"
	"github.com/pkg/errors"
	"github.com/vbatts/go-mtree"
	"golang.org/x/sys/unix"
)

// ExcludePaths represents a list of paths to exclude in a squashfs listing.
// Users should do something like filepath.Walk() over the whole filesystem,
// calling AddExclude() or AddInclude() based on whether they want to include
// or exclude a particular file. Note that if e.g. /usr is excluded, then
// everyting underneath is also implicitly excluded. The
// AddExclude()/AddInclude() methods do the math to figure out what is the
// correct set of things to exclude or include based on what paths have been
// previously included or excluded.
type ExcludePaths struct {
	exclude map[string]bool
	include []string
}

func NewExcludePaths() *ExcludePaths {
	return &ExcludePaths{
		exclude: map[string]bool{},
		include: []string{},
	}
}

func (eps *ExcludePaths) AddExclude(p string) {
	for _, inc := range eps.include {
		// If /usr/bin/ls has changed but /usr hasn't, we don't want to list
		// /usr in the include paths any more, so let's be sure to only
		// add things which aren't prefixes.
		if strings.HasPrefix(inc, p) {
			return
		}
	}
	eps.exclude[p] = true
}

func (eps *ExcludePaths) AddInclude(orig string, isDir bool) {
	// First, remove this thing and all its parents from exclude.
	p := orig

	// normalize to the first dir
	if !isDir {
		p = path.Dir(p)
	}
	for {
		// our paths are all absolute, so this is a base case
		if p == "/" {
			break
		}

		delete(eps.exclude, p)
		p = path.Dir(p)
	}

	// now add it to the list of includes, so we don't accidentally re-add
	// anything above.
	eps.include = append(eps.include, orig)
}

func (eps *ExcludePaths) String() (string, error) {
	var buf bytes.Buffer
	for p := range eps.exclude {
		_, err := buf.WriteString(p)
		if err != nil {
			return "", err
		}
		_, err = buf.WriteString("\n")
		if err != nil {
			return "", err
		}
	}

	_, err := buf.WriteString("\n")
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func MakeSquashfs(tempdir string, rootfs string, eps *ExcludePaths) (io.ReadCloser, error) {
	var excludesFile string
	var err error
	var toExclude string

	if eps != nil {
		toExclude, err = eps.String()
		if err != nil {
			return nil, errors.Wrapf(err, "couldn't create exclude path list")
		}
	}

	if len(toExclude) != 0 {
		excludes, err := ioutil.TempFile(tempdir, "stacker-squashfs-exclude-")
		if err != nil {
			return nil, err
		}
		defer os.Remove(excludes.Name())

		excludesFile = excludes.Name()
		_, err = excludes.WriteString(toExclude)
		excludes.Close()
		if err != nil {
			return nil, err
		}
	}

	tmpSquashfs, err := ioutil.TempFile(tempdir, "stacker-squashfs-img-")
	if err != nil {
		return nil, err
	}
	tmpSquashfs.Close()
	os.Remove(tmpSquashfs.Name())
	defer os.Remove(tmpSquashfs.Name())
	args := []string{rootfs, tmpSquashfs.Name()}
	if len(toExclude) != 0 {
		args = append(args, "-ef", excludesFile)
	}
	cmd := exec.Command("mksquashfs", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		return nil, errors.Wrap(err, "couldn't build squashfs")
	}

	return os.Open(tmpSquashfs.Name())
}

func GenerateSquashfsLayer(name, author, bundlepath, ocidir string, oci casext.Engine) error {
	meta, err := umoci.ReadBundleMeta(bundlepath)
	if err != nil {
		return err
	}

	mtreeName := strings.Replace(meta.From.Descriptor().Digest.String(), ":", "_", 1)
	mtreePath := path.Join(bundlepath, mtreeName+".mtree")

	mfh, err := os.Open(mtreePath)
	if err != nil {
		return errors.Wrapf(err, "opening mtree")
	}

	spec, err := mtree.ParseSpec(mfh)
	if err != nil {
		return err
	}

	fsEval := fseval.Rootless
	rootfsPath := path.Join(bundlepath, "rootfs")
	newDH, err := mtree.Walk(rootfsPath, nil, umoci.MtreeKeywords, fsEval)
	if err != nil {
		return errors.Wrapf(err, "couldn't mtree walk %s", rootfsPath)
	}

	diffs, err := mtree.CompareSame(spec, newDH, umoci.MtreeKeywords)
	if err != nil {
		return err
	}

	diffs = mtreefilter.FilterDeltas(diffs,
		stackermtree.LayerGenerationIgnoreRoot,
		mtreefilter.SimplifyFilter(diffs))

	// This is a pretty massive hack, because there's no library for
	// generating squashfs images. However, mksquashfs does take a list of
	// files to exclude from the image. So we go through and accumulate a
	// list of these files.
	//
	// For missing files, since we're going to use overlayfs with
	// squashfs, we use overlayfs' mechanism for whiteouts, which is a
	// character device with device numbers 0/0. But since there's no
	// library for generating squashfs images, we have to write these to
	// the actual filesystem, and then remember what they are so we can
	// delete them later.
	missing := []string{}
	defer func() {
		for _, f := range missing {
			os.Remove(f)
		}
	}()

	// we only need to generate a layer if anything was added, modified, or
	// deleted; if everything is the same this should be a no-op.
	needsLayer := false
	paths := NewExcludePaths()
	for _, diff := range diffs {
		switch diff.Type() {
		case mtree.Modified, mtree.Extra:
			needsLayer = true
			p := path.Join(rootfsPath, diff.Path())
			paths.AddInclude(p, diff.New().IsDir())
		case mtree.Missing:
			needsLayer = true
			p := path.Join(rootfsPath, diff.Path())
			missing = append(missing, p)
			paths.AddInclude(p, diff.Old().IsDir())
			if err := unix.Mknod(p, unix.S_IFCHR, int(unix.Mkdev(0, 0))); err != nil {
				if !os.IsNotExist(err) && err != unix.ENOTDIR {
					// No privilege to create device nodes. Create a .wh.$filename instead.
					dirname := path.Dir(diff.Path())
					fname := fmt.Sprintf(".wh.%s", path.Base(diff.Path()))
					whPath := path.Join(rootfsPath, dirname, fname)
					fd, err := os.Create(whPath)
					if err != nil {
						return errors.Wrapf(err, "couldn't mknod whiteout for %s", diff.Path())
					}
					fd.Close()
				}
			}
		case mtree.Same:
			paths.AddExclude(path.Join(rootfsPath, diff.Path()))
		}
	}

	if !needsLayer {
		return nil
	}

	tmpSquashfs, err := MakeSquashfs(ocidir, rootfsPath, paths)
	if err != nil {
		return err
	}
	defer tmpSquashfs.Close()

	desc, err := stackeroci.AddBlobNoCompression(oci, name, tmpSquashfs)
	if err != nil {
		return err
	}

	newName := strings.Replace(desc.Digest.String(), ":", "_", 1) + ".mtree"
	err = umoci.GenerateBundleManifest(newName, bundlepath, fsEval)
	if err != nil {
		return err
	}

	os.Remove(mtreePath)
	meta.From = casext.DescriptorPath{
		Walk: []ispec.Descriptor{desc},
	}
	err = umoci.WriteBundleMeta(bundlepath, meta)
	if err != nil {
		return err
	}

	return nil
}

func ExtractSingleSquash(squashFile string, extractDir string, storageType string) error {
	err := os.MkdirAll(extractDir, 0755)
	if err != nil {
		return err
	}

	var uCmd []string
	if storageType == "btrfs" {
		if which("squashtool") == "" {
			return errors.Errorf("must have squashtool (https://github.com/anuvu/squashfs) to correctly extract squashfs using btrfs storage backend")
		}

		uCmd = []string{"squashtool", "extract", "--whiteouts", "--perms",
			"--devs", "--sockets", "--owners"}
		uCmd = append(uCmd, squashFile, extractDir)
	} else {
		uCmd = []string{"unsquashfs", "-f", "-d", extractDir, squashFile}
	}

	cmd := exec.Command(uCmd[0], uCmd[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func which(name string) string {
	return whichSearch(name, strings.Split(os.Getenv("PATH"), ":"))
}

func whichSearch(name string, paths []string) string {
	var search []string

	if strings.ContainsRune(name, os.PathSeparator) {
		if path.IsAbs(name) {
			search = []string{name}
		} else {
			search = []string{"./" + name}
		}
	} else {
		search = []string{}
		for _, p := range paths {
			search = append(search, path.Join(p, name))
		}
	}

	for _, fPath := range search {
		if err := unix.Access(fPath, unix.X_OK); err == nil {
			return fPath
		}
	}

	return ""
}
