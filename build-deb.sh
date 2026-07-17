#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

package_name="grok-switch"
app_name="grok_switch"
version="${VERSION:-0.4.2}"
arch="${ARCH:-amd64}"
maintainer="${MAINTAINER:-grok_switch maintainers}"
build_root="dist/deb/${package_name}_${version}_${arch}"
deb_path="dist/${package_name}_${version}_${arch}.deb"

./build-linux.sh

rm -rf "${build_root}"
mkdir -p \
  "${build_root}/DEBIAN" \
  "${build_root}/usr/bin" \
  "${build_root}/usr/share/applications" \
  "${build_root}/usr/share/icons/hicolor/256x256/apps" \
  "${build_root}/usr/share/icons/hicolor/scalable/apps" \
  "${build_root}/usr/share/doc/${package_name}"

install -m 0755 "dist/linux/${app_name}" "${build_root}/usr/bin/${app_name}"
install -m 0644 "assets/icon.png" "${build_root}/usr/share/icons/hicolor/256x256/apps/${app_name}.png"
install -m 0644 "icon.svg" "${build_root}/usr/share/icons/hicolor/scalable/apps/${app_name}.svg"
install -m 0644 "LICENSE" "${build_root}/usr/share/doc/${package_name}/copyright"

cat > "${build_root}/usr/share/applications/${app_name}.desktop" <<EOF_DESKTOP
[Desktop Entry]
Type=Application
Name=grok_switch
Comment=Grok Build profile switcher
Exec=/usr/bin/${app_name}
Icon=${app_name}
Terminal=false
Categories=Utility;Development;
StartupNotify=false
EOF_DESKTOP

cat > "${build_root}/DEBIAN/control" <<EOF_CONTROL
Package: ${package_name}
Version: ${version}
Section: utils
Priority: optional
Architecture: ${arch}
Maintainer: ${maintainer}
Depends: libc6, libayatana-appindicator3-1 | libappindicator3-1, libgtk-3-0
Recommends: libnotify-bin, xdg-utils, xclip | xsel
Description: Local tray tool for switching Grok Build profiles
 grok_switch manages Grok CLI config.toml profiles from a local tray app
 and embedded web management panel.
EOF_CONTROL

dpkg-deb --build --root-owner-group "${build_root}" "${deb_path}"
sha256sum "${deb_path}" > "${deb_path}.sha256"

printf 'Built %s\n' "${deb_path}"
printf 'SHA-256 %s\n' "${deb_path}.sha256"
