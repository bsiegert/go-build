#!/usr/bin/env python
# Copyright 2016 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

import anita
import sys

arch = sys.argv[1]
release = sys.argv[2]
pkg_release = sys.argv[3]

pkg_path = "https://cdn.netbsd.org/pub/pkgsrc/packages/NetBSD/{}/{}/All/".format(arch, pkg_release)


install_packages = [
    "bash",
    "curl",
    "git-base",
    "go122",
    # Interactive debugging tools for users using gomote ssh
    "emacs29-nox11",
    "vim",
    "screen",
    # For https://golang.org/issue/24354
    "clang",
    "cmake",
]

commands = [
    """cat >> /etc/rc.local <<EOF
(
  export PATH=/usr/pkg/bin:/usr/pkg/sbin:${PATH}
  export GOROOT_BOOTSTRAP=/usr/pkg/go121
  set -x
  echo 'starting buildlet script'
  netstat -rn
  cat /etc/resolv.conf
  dig metadata.google.internal
  (
    set -e
    curl -o /buildlet \$(curl -H 'Metadata-Flavor: Google' http://metadata.google.internal/computeMetadata/v1/instance/attributes/buildlet-binary-url)
    chmod +x /buildlet
    exec /buildlet
  )
  echo 'giving up'
  sleep 10
  halt -p
)
EOF""",
    """cat > /etc/ifconfig.vioif0 << EOF
!dhcpcd
mtu 1460
EOF""",
    "dhcpcd -w",
    "env PKG_PATH={} pkg_add pkgin".format(pkg_path),
    "echo {} > /usr/pkg/etc/pkgin/repositories.conf".format(pkg_path),
    "pkgin update",
    "pkgin -y install {}".format(" ".join(install_packages)),
    "pkgin clean",

    # Remove the /tmp entry, because it's mounted as tmpfs -s=ram%25 by default, which isn't enough disk space.
    """ed /etc/fstab << EOF
H
/\\/tmp/d
wq
EOF""",

    "echo sshd=yes >> /etc/rc.conf",
    "echo PermitRootLogin without-password >> /etc/ssh/sshd_config",
    "/etc/rc.d/sshd restart",
    "sync; shutdown -hp now",
]

a = anita.Anita(
    anita.URL('https://cdn.netbsd.org/pub/NetBSD/NetBSD-{}/{}/'.format(release, arch)),
    workdir="work-NetBSD-{}-{}".format(release, arch),
    disk_size="16G",
    memory_size="2G",
    persist=True)
child = a.boot()
anita.login(child)

for cmd in commands:
  anita.shell_cmd(child, cmd, 3600)

# Sometimes, the halt command times out, even though it has completed
# successfully.
try:
    a.halt()
except:
    pass
