load helpers

function setup() {
    stacker_setup
    unpriv_setup
}

function teardown() {
    cleanup
}

@test "file with chmod 000 works" {
    cat > stacker.yaml <<EOF
parent:
    from:
        type: oci
        url: $CENTOS_OCI
    run: |
        touch /etc/000
        chmod 000 /etc/000
child:
    from:
        type: oci
        url: $CENTOS_OCI
    run: |
        echo "zomg" > /etc/000
        chmod 000 /etc/000
EOF
    unpriv_stacker build
    umoci unpack --image oci:parent parent
    [ -f parent/rootfs/etc/000 ]
    [ "$(stat --format="%a" parent/rootfs/etc/000)" = "0" ]

    umoci unpack --image oci:child child
    [ -f child/rootfs/etc/000 ]
    [ "$(stat --format="%a" child/rootfs/etc/000)" = "0" ]
    [ "$(cat child/rootfs/etc/000)" = "zomg" ]
}

@test "unprivileged stacker" {
    cat > stacker.yaml <<EOF
centos:
    from:
        type: oci
        url: $CENTOS_OCI
    import:
        - https://www.cisco.com/favicon.ico
    run: |
        cp /stacker/favicon.ico /favicon.ico
layer1:
    from:
        type: built
        tag: centos
    run:
        - rm /favicon.ico
EOF
    unpriv_stacker build
    umoci unpack --image oci:centos centos
    [ "$(sha .stacker/imports/centos/favicon.ico)" == "$(sha centos/rootfs/favicon.ico)" ]
    umoci unpack --image oci:layer1 layer1
    [ ! -f layer1/rootfs/favicon.ico ]
}

@test "unprivileged read-only imports can be re-cached" {
    sudo -s -u $SUDO_USER <<EOF
mkdir -p import
touch import/this
chmod -w import
EOF

    cat > stacker.yaml <<EOF
centos:
    from:
        type: oci
        url: $CENTOS_OCI
    import:
        - import
EOF
    unpriv_stacker build
    ls -al import import/*
    echo that | sudo -u $SUDO_USER tee import/this
    unpriv_stacker build
}

@test "/stacker in unprivileged mode gets deleted" {
    sudo -s -u $SUDO_USER <<EOF
touch first
touch second
EOF

    cat > stacker.yaml <<EOF
base:
    from:
        type: oci
        url: $CENTOS_OCI
    import:
        - first
        - second
    run: |
        ls -alh /stacker
        tar -C /stacker -cv -f /base.tar.gz first second
next:
    from:
        type: tar
        url: stacker://base/base.tar.gz
EOF
    unpriv_stacker build

    umoci unpack --image oci:base base
    [ ! -d base/rootfs/stacker ]

    umoci unpack --image oci:next next
    [ -f next/rootfs/first ]
    [ -f next/rootfs/second ]
    [ ! -d next/rootfs/stacker ]
}

@test "stacker switching privilege modes fails" {
    cat > stacker.yaml <<EOF
base:
    from:
        type: oci
        url: $CENTOS_OCI
    import:
        - test
    run: cat /stacker/test
EOF
    echo unpriv | sudo -s -u $SUDO_USER tee test
    unpriv_stacker build
    echo priv > test
    bad_stacker build
}

@test "underlying layer output conversion happens in a user namespace" {
    require_storage overlay

    cat > stacker.yaml <<EOF
image:
    from:
        type: oci
        url: $CENTOS_OCI
EOF

    unpriv_stacker build --layer-type squashfs
    manifest=$(cat oci/index.json | jq -r .manifests[0].digest | cut -f2 -d:)
    layer0=$(cat oci/blobs/sha256/$manifest | jq -r .layers[0].digest | cut -f2 -d:)

    mkdir layer0
    mount -t squashfs oci/blobs/sha256/$layer0 layer0
    echo "mount has uid $(stat --format "%u" layer0/usr/bin/mount)"
    [ "$(stat --format "%u" layer0/usr/bin/mount)" = "0" ]
}
