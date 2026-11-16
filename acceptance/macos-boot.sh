#!/usr/bin/env bash
# Boots the macOS acceptance VM with OSX-KVM (https://github.com/kholia/OSX-KVM) in plain QEMU.
# dockur/macos needs internet inside Docker, which the cockpit firewall blocks; QEMU user
# networking needs no forwarding. The VM runs alone, never together with other VMs.
#
#   acceptance/macos-boot.sh install   # boot with the recovery image (GUI install over VNC 127.0.0.1:5931)
#   acceptance/macos-boot.sh run       # boot the installed disk
#   acceptance/macos-boot.sh snapshot  # boot the installed disk; QEMU discards all changes
#
# Files in ~/.cache/fam-acceptance/macos: OSX-KVM/ (clone), dl/BaseSystem.img
# (fetch-macOS-v2.py -s sonoma, then dmg2img), mac_hdd.qcow2 (80 GB).
set -euo pipefail
D="$HOME/.cache/fam-acceptance/macos"
K="$D/OSX-KVM"
mode="${1:-run}"
cp -n "$K/OVMF_VARS-1920x1080.fd" "$D/OVMF_VARS.fd" 2>/dev/null || true
args=(
  -enable-kvm -m 6144
  -cpu "Skylake-Client,-hle,-rtm,kvm=on,vendor=GenuineIntel,+invtsc,vmware-cpuid-freq=on,+ssse3,+sse4.2,+popcnt,+avx,+aes,+xsave,+xsaveopt,check"
  -machine q35 -smp 4,cores=2,sockets=1
  -device qemu-xhci,id=xhci -device usb-kbd,bus=xhci.0 -device usb-tablet,bus=xhci.0
  -device isa-applesmc,osk="ourhardworkbythesewordsguardedpleasedontsteal(c)AppleComputerInc"
  -drive if=pflash,format=raw,readonly=on,file="$K/OVMF_CODE_4M.fd"
  -drive if=pflash,format=raw,file="$D/OVMF_VARS.fd"
  -smbios type=2
  -device ich9-ahci,id=sata
  -drive id=OpenCoreBoot,if=none,snapshot=on,format=qcow2,file="$K/OpenCore/OpenCore.qcow2"
  -device ide-hd,bus=sata.2,drive=OpenCoreBoot
  -drive id=MacHDD,if=none,file="$D/mac_hdd.qcow2",format=qcow2
  -device ide-hd,bus=sata.4,drive=MacHDD
  -netdev user,id=net0,hostfwd=tcp:127.0.0.1:2267-:22 -device virtio-net-pci,netdev=net0,id=nic0,mac=52:54:00:c9:18:27
  -device vmware-svga
  -display none -vnc 127.0.0.1:31
  -qmp unix:"$D/qmp.sock",server=on,wait=off
)
if [ "$mode" = snapshot ]; then
  args+=(-snapshot)
fi
if [ "$mode" = install ]; then
  args+=(-drive id=InstallMedia,if=none,file="$D/dl/BaseSystem.img",format=raw -device ide-hd,bus=sata.3,drive=InstallMedia)
fi
exec qemu-system-x86_64 "${args[@]}"
