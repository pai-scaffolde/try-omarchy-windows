/*
 * Runs QEMU's real whpx_report_free_pages() against real Windows memory while
 * this process is held below its own commit charge by a job object. That is
 * the state a host reaches when something else takes commit while QEMU is
 * reclaiming: any step that gives commit back and then asks for it again
 * fails with ERROR_COMMITMENT_LIMIT. The limit applies to this test process
 * only, so the host never comes under memory pressure.
 *
 * The reclaim must return, unpin exactly the reported range before discarding
 * it, never unmap or remap the range, keep it committed, release its physical
 * pages, and survive a failed unpin or discard by logging it. Windows documents
 * discarded contents as unspecified; the harness also checks that this build
 * leaves each page with its old contents or zeroes, so a Windows change shows.
 *
 * The WHP calls are stubs, so this covers Windows commit charge, working-set
 * residency and call order. Guest and host coherence through a real partition
 * needs WHPX, which hosted runners lack; the PR's scratch probes cover it.
 */
#include <windows.h>
#include <psapi.h>
#include <assert.h>
#include <inttypes.h>
#include <setjmp.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define PAGE 4096u
#define CHUNK (2u << 20)
#define CHUNKS 16u
#define GPA_BASE 0x40000000ull

typedef uint64_t hwaddr;
typedef struct { uint64_t lo; } Int128;
typedef struct { int fd; unsigned flags; } RAMBlock;
typedef struct { RAMBlock *ram_block; bool readonly, rom_device; } MemoryRegion;
typedef struct {
    MemoryRegion *mr;
    Int128 size;
    hwaddr offset_within_address_space, offset_within_region;
} MemoryRegionSection;
typedef void *WHV_PARTITION_HANDLE;
enum { RAM_SHARED = 1, RAM_PREALLOC = 2 };
enum {
    WHvMapGpaRangeFlagRead = 1,
    WHvMapGpaRangeFlagWrite = 2,
    WHvMapGpaRangeFlagExecute = 4,
};
typedef struct { uint64_t GuestAddress, SizeInBytes; } WHV_MEMORY_RANGE_ENTRY;
typedef enum { WHvAdviseGpaRangeCodeUnpin = 2 } WHV_ADVISE_GPA_RANGE_CODE;

static unsigned char *ram;
static RAMBlock block = { .fd = -1 };
static MemoryRegion region = { .ram_block = &block };
static int unmapped, mapped, refs;
static bool fail_discard, fail_unpin;
static int discards, unpins, unpinned_before_discard;
static hwaddr unpin_gpa;
static uint64_t unpin_size;
static char last_message[512];
static int messages;
static jmp_buf aborted;

static HRESULT unmap_gpa(WHV_PARTITION_HANDLE p, hwaddr gpa, uint64_t size)
{
    (void)p; (void)gpa; (void)size;
    unmapped++;
    return S_OK;
}

static HRESULT map_gpa(WHV_PARTITION_HANDLE p, void *host, hwaddr gpa,
                       uint64_t size, int flags)
{
    (void)p; (void)host; (void)gpa; (void)size; (void)flags;
    mapped++;
    return S_OK;
}

/* Large RAM mappings are pinned; a pinned page is only released once unpinned. */
static HRESULT advise_gpa(WHV_PARTITION_HANDLE p, const WHV_MEMORY_RANGE_ENTRY *ranges,
                          UINT32 count, WHV_ADVISE_GPA_RANGE_CODE advice,
                          const void *buffer, UINT32 buffer_size)
{
    (void)p; (void)buffer; (void)buffer_size;
    if (count != 1 || advice != WHvAdviseGpaRangeCodeUnpin) {
        return E_INVALIDARG;
    }
    unpins++;
    unpin_gpa = ranges[0].GuestAddress;
    unpin_size = ranges[0].SizeInBytes;
    return fail_unpin ? E_OUTOFMEMORY : S_OK;
}

static __attribute__((unused)) struct {
    HRESULT (*WHvUnmapGpaRange)(WHV_PARTITION_HANDLE, hwaddr, uint64_t);
    HRESULT (*WHvMapGpaRange)(WHV_PARTITION_HANDLE, void *, hwaddr, uint64_t, int);
    HRESULT (*WHvAdviseGpaRange)(WHV_PARTITION_HANDLE, const WHV_MEMORY_RANGE_ENTRY *,
                                 UINT32, WHV_ADVISE_GPA_RANGE_CODE, const void *, UINT32);
} whp_dispatch = { unmap_gpa, map_gpa, advise_gpa };
static __attribute__((unused)) struct { WHV_PARTITION_HANDLE partition; } whpx_global;

static bool bql_locked(void) { return true; }
static size_t qemu_real_host_page_size(void) { return PAGE; }
static void *get_system_memory(void) { return NULL; }
static MemoryRegionSection memory_region_find(void *as, hwaddr gpa, uint64_t size)
{
    (void)as;
    refs++;
    return (MemoryRegionSection){
        .mr = &region, .size = { size }, .offset_within_address_space = gpa,
        .offset_within_region = gpa - GPA_BASE,
    };
}
static bool memory_region_is_ram(MemoryRegion *mr) { (void)mr; return true; }
static bool memory_region_is_ram_device(MemoryRegion *mr) { (void)mr; return false; }
static unsigned memory_region_get_dirty_log_mask(MemoryRegion *mr) { (void)mr; return 0; }
static void *memory_region_get_ram_ptr(MemoryRegion *mr) { (void)mr; return ram; }
static void memory_region_unref(MemoryRegion *mr) { (void)mr; refs--; }
static uint64_t int128_get64(Int128 v) { return v.lo; }
static bool is_power_of_2(uint64_t v) { return v && !(v & (v - 1)); }
#define QEMU_IS_ALIGNED(n, m) (((n) % (m)) == 0)

static void record(const char *fmt, va_list ap)
{
    vsnprintf(last_message, sizeof(last_message), fmt, ap);
    messages++;
    fprintf(stderr, "qemu: %s\n", last_message);
}
static void error_report(const char *fmt, ...)
{
    va_list ap;
    va_start(ap, fmt);
    record(fmt, ap);
    va_end(ap);
}
static void warn_report(const char *fmt, ...)
{
    va_list ap;
    va_start(ap, fmt);
    record(fmt, ap);
    va_end(ap);
}
static void harness_abort(void) { longjmp(aborted, 1); }

typedef DWORD (WINAPI *DiscardFn)(PVOID, SIZE_T);
static DiscardFn real_discard;
static DWORD WINAPI discard_or_fail(PVOID address, SIZE_T size)
{
    discards++;
    unpinned_before_discard += unpins == discards;
    return fail_discard ? ERROR_NOT_ENOUGH_MEMORY : real_discard(address, size);
}
static FARPROC harness_get_proc_address(HMODULE module, LPCSTR name)
{
    if (!strcmp(name, "DiscardVirtualMemory")) {
        real_discard = (DiscardFn)(void (*)(void))GetProcAddress(module, name);
        return real_discard ? (FARPROC)(void (*)(void))discard_or_fail : NULL;
    }
    return GetProcAddress(module, name);
}

#define abort() harness_abort()
#define GetProcAddress(module, name) harness_get_proc_address(module, name)
#include "free-page-reclaim-under-test.c"
#undef GetProcAddress
#undef abort

static PSAPI_WORKING_SET_EX_INFORMATION pages[CHUNK / PAGE];

static size_t private_usage(void)
{
    PROCESS_MEMORY_COUNTERS_EX counters = { .cb = sizeof(counters) };
    GetProcessMemoryInfo(GetCurrentProcess(), (PROCESS_MEMORY_COUNTERS *)&counters,
                         sizeof(counters));
    return counters.PrivateUsage;
}

static size_t resident(unsigned char *base)
{
    size_t n = 0;
    for (size_t i = 0; i < CHUNK / PAGE; i++) {
        pages[i].VirtualAddress = base + i * PAGE;
    }
    if (!QueryWorkingSetEx(GetCurrentProcess(), pages, sizeof(pages))) {
        fprintf(stderr, "QueryWorkingSetEx failed: %lu\n", GetLastError());
        exit(2);
    }
    for (size_t i = 0; i < CHUNK / PAGE; i++) {
        n += pages[i].VirtualAttributes.Valid;
    }
    return n;
}

static bool committed(unsigned char *base, size_t size)
{
    MEMORY_BASIC_INFORMATION info;
    return VirtualQuery(base, &info, sizeof(info)) && info.State == MEM_COMMIT &&
           info.Protect == PAGE_READWRITE && info.RegionSize >= size;
}

#define CHECK(cond, ...) do { if (!(cond)) { \
    fprintf(stderr, "not ok - " __VA_ARGS__); fputc('\n', stderr); return 1; } } while (0)

static int reclaim(unsigned char *chunk)
{
    if (setjmp(aborted)) {
        fprintf(stderr, "not ok - the free-page reclaim aborted QEMU: %s\n", last_message);
        return 1;
    }
    whpx_report_free_pages(GPA_BASE + (hwaddr)(chunk - ram), chunk, CHUNK);
    return 0;
}

int main(void)
{
    setvbuf(stdout, NULL, _IONBF, 0);
    setvbuf(stderr, NULL, _IONBF, 0);
    ram = VirtualAlloc(NULL, CHUNK * CHUNKS, MEM_RESERVE | MEM_COMMIT, PAGE_READWRITE);
    CHECK(ram, "guest RAM allocation failed: %lu", GetLastError());
    memset(ram, 0x5a, CHUNK * CHUNKS);

    /* Hold the process below its own commit charge. */
    size_t before = private_usage();
    HANDLE job = CreateJobObjectW(NULL, NULL);
    JOBOBJECT_EXTENDED_LIMIT_INFORMATION limit = { 0 };
    limit.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_PROCESS_MEMORY;
    limit.ProcessMemoryLimit = before - 4 * CHUNK;
    CHECK(job && SetInformationJobObject(job, JobObjectExtendedLimitInformation,
                                         &limit, sizeof(limit)) &&
          AssignProcessToJobObject(job, GetCurrentProcess()),
          "job commit limit could not be applied: %lu", GetLastError());

    /* Control: giving commit back and asking for it again must now fail. */
    unsigned char *control = ram + (CHUNKS - 1) * CHUNK;
    CHECK(VirtualFree(control, CHUNK, MEM_DECOMMIT), "control decommit failed");
    CHECK(!VirtualAlloc(control, CHUNK, MEM_COMMIT, PAGE_READWRITE),
          "the commit limit did not bite; the test proves nothing");
    DWORD control_error = GetLastError();
    CHECK(control_error == ERROR_COMMITMENT_LIMIT,
          "control recommit failed with %lu, expected %lu", control_error,
          (unsigned long)ERROR_COMMITMENT_LIMIT);
    printf("ok - control decommit and recommit fail with %lu under the job limit\n",
           control_error);

    /* A reclaim under that limit returns and gives physical pages back. */
    unsigned char *chunk = ram;
    size_t charged = private_usage();
    size_t was_resident = resident(chunk);
    CHECK(was_resident == CHUNK / PAGE, "test chunk was not resident");
    if (reclaim(chunk)) {
        return 1;
    }
    CHECK(!refs, "memory region reference leaked");
    CHECK(unpins == 1 && unpin_gpa == GPA_BASE && unpin_size == CHUNK,
          "the reported range was not unpinned exactly (%d unpins, %llx+%llx)", unpins,
          (unsigned long long)unpin_gpa, (unsigned long long)unpin_size);
    CHECK(unpinned_before_discard == 1, "the range was discarded before it was unpinned");
    CHECK(!unmapped && !mapped, "the reclaim unmapped or remapped the GPA range "
          "(%d unmaps, %d maps)", unmapped, mapped);
    CHECK(committed(chunk, CHUNK), "reclaimed range is no longer committed");
    CHECK(private_usage() == charged, "commit charge changed: %zu -> %zu",
          charged, private_usage());
    size_t now_resident = resident(chunk);
    CHECK(now_resident < was_resident / 8, "physical pages stayed resident: %zu of %zu",
          now_resident, was_resident);
    printf("ok - reclaim returned under the limit, kept its commit charge, "
           "released %zu of %zu pages\n", was_resident - now_resident, was_resident);

    for (size_t i = 0; i < CHUNK; i += PAGE) {
        unsigned char first = chunk[i];
        CHECK(first == 0x5a || first == 0, "page %zu holds foreign data", i / PAGE);
        for (size_t j = 1; j < PAGE; j++) {
            CHECK(chunk[i + j] == first, "page %zu is partly changed", i / PAGE);
        }
    }
    memset(chunk, 0x33, CHUNK);
    CHECK(chunk[CHUNK - 1] == 0x33, "reclaimed range is not writable");
    printf("ok - every reclaimed page reads its old contents or zeroes and is writable\n");

    /* A failed discard is logged sparingly and QEMU keeps running. */
    fail_discard = true;
    messages = 0;
    for (unsigned i = 1; i <= 3; i++) {
        if (reclaim(ram + i * CHUNK)) {
            return 1;
        }
    }
    CHECK(messages == 2, "expected 2 logged failures out of 3, got %d", messages);
    CHECK(strstr(last_message, "discard failed"), "failure log does not say why: %s",
          last_message);
    CHECK(resident(ram + CHUNK) == CHUNK / PAGE, "failed discard lost resident pages");
    CHECK(committed(ram + CHUNK, CHUNK), "failed discard left the range uncommitted");
    printf("ok - failed discards are logged, leave pages resident, and return\n");

    /* A failed unpin is logged and the reclaim still returns. */
    fail_discard = false;
    fail_unpin = true;
    messages = 0;
    if (reclaim(ram + 4 * CHUNK)) {
        return 1;
    }
    CHECK(messages == 1 && strstr(last_message, "unpin failed"),
          "a failed unpin was not logged: %s", last_message);
    CHECK(committed(ram + 4 * CHUNK, CHUNK), "failed unpin left the range uncommitted");
    printf("ok - a failed unpin is logged and the reclaim returns\n");
    return 0;
}
