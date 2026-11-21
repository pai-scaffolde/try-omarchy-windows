#!/usr/bin/env bash
# Compile QEMU's actual WHPX free-page reclaim into a Windows harness and run
# it under a per-process commit limit. Usage: test-free-page-reclaim.sh <qemu-source>
set -euo pipefail

recipe=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source_file="$1/accel/whpx/whpx-common.c"
balloon="$1/hw/virtio/virtio-balloon.c"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The reclaim treats a free page's contents as disposable. That only holds
# while the device skips guests that asked for reported pages to keep a
# poison value, and returns every report whether or not it was reclaimed.
grep -Fq 'if (virtio_balloon_inhibited() || dev->poison_val) {' "$balloon"
awk '/^static void virtio_balloon_handle_report\(/,/^}/' "$balloon" > "$work/report.c"
grep -Fq 'whpx_report_free_pages(elem->in_addr[i], addr, size);' "$work/report.c"
grep -Eq '^skip_element:$' "$work/report.c"
awk '/^skip_element:$/,/^}/' "$work/report.c" | grep -Fq 'virtqueue_push(vq, elem, 0);'

# Everything from the reclaim's helpers through the end of the function.
awk '
    /^typedef DWORD \(WINAPI \*WhpxDiscardVirtualMemory\)/ { on = 1 }
    /^void whpx_report_free_pages\(hwaddr gpa, void \*host, size_t size\)$/ { on = 1; fn = 1 }
    on { print }
    fn && /^}$/ { exit }
' "$source_file" > "$work/free-page-reclaim-under-test.c"
grep -q '^void whpx_report_free_pages(' "$work/free-page-reclaim-under-test.c"

cp "$recipe/free-page-reclaim-harness.c" "$work/"
gcc -std=gnu11 -O1 -Wall -Werror -Wno-unused-function -I"$work" \
    -o "$work/free-page-reclaim.exe" "$work/free-page-reclaim-harness.c" -lpsapi
"$work/free-page-reclaim.exe"
