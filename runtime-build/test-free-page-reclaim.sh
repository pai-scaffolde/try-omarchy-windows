#!/usr/bin/env bash
# Compile QEMU's actual WHPX free-page reclaim into a Windows harness and run
# it under a per-process commit limit. Usage: test-free-page-reclaim.sh <qemu-source>
set -euo pipefail

recipe=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source_file="$1/accel/whpx/whpx-common.c"
balloon="$1/hw/virtio/virtio-balloon.c"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail() { echo "not ok - $*" >&2; exit 1; }

# Everything from the reclaim's helpers through the end of the function.
awk '
    /^typedef DWORD \(WINAPI \*WhpxDiscardVirtualMemory\)/ { on = 1 }
    /^void whpx_report_free_pages\(hwaddr gpa, void \*host, size_t size\)$/ { on = 1; fn = 1 }
    on { print }
    fn && /^}$/ { exit }
' "$source_file" > "$work/free-page-reclaim-under-test.c"
grep -q '^void whpx_report_free_pages(' "$work/free-page-reclaim-under-test.c" ||
    fail "whpx_report_free_pages not found in $source_file"

cp "$recipe/free-page-reclaim-harness.c" "$work/"
gcc -std=gnu11 -O1 -Wall -Werror -Wno-unused-function -I"$work" \
    -o "$work/free-page-reclaim.exe" "$work/free-page-reclaim-harness.c" -lpsapi
"$work/free-page-reclaim.exe"

# The reclaim treats a free page's contents as disposable. That only holds
# while WHPX reclaims only for guests that did not negotiate PAGE_POISON, and
# the device returns every report whether or not it was reclaimed.
awk '/^static void virtio_balloon_handle_report\(/,/^}/' "$balloon" > "$work/report.c"
grep -Fq 'if (virtio_balloon_inhibited() || dev->poison_val) {' "$work/report.c" ||
    fail "the report handler no longer skips a non-zero poison value"
tr -s ' \n' ' ' < "$work/report.c" | grep -Fq \
    'if (vdev->dma_as == &address_space_memory && !virtio_vdev_has_feature(vdev, VIRTIO_BALLOON_F_PAGE_POISON)) { whpx_report_free_pages(elem->in_addr[i], addr, size);' ||
    fail "WHPX reclaims reported pages for a guest that negotiated PAGE_POISON"
awk '/^skip_element:$/,/^}/' "$work/report.c" | grep -Fq 'virtqueue_push(vq, elem, 0);' ||
    fail "a report is not always returned to the guest"
echo "ok - WHPX reclaims only without PAGE_POISON and every report is returned"
