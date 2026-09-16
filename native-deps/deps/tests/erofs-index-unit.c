/* SPDX-License-Identifier: Apache-2.0
 * Include the candidate's actual private chunk implementation. Fault injection
 * exists only in this test executable, via the linker's syscall wrappers.
 */
#define _GNU_SOURCE
#include "blobchunk.c"
#include <assert.h>
#include <dirent.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/vfs.h>
#include <linux/magic.h>
#include <stdarg.h>

static int reserve_error, map_error, create_error, heap_error, malloc_error;
static long filesystem_type;
static unsigned int reserve_calls, live_maps;
static int reserved_fd = -1;
static struct { void *p; size_t bytes; } mappings[128];

int __real_fallocate64(int fd, int mode, off64_t offset, off64_t length);
int __wrap_fallocate64(int fd, int mode, off64_t offset, off64_t length)
{
	++reserve_calls;
	reserved_fd = -1;
	if (reserve_error) {
		errno = reserve_error;
		return -1;
	}
	int ret = __real_fallocate64(fd, mode, offset, length);
	if (!ret)
		reserved_fd = fd;
	return ret;
}

int __real_openat64(int fd, const char *path, int flags, ...);
int __wrap_openat64(int fd, const char *path, int flags, ...)
{
	va_list args;
	va_start(args, flags);
	mode_t mode = va_arg(args, int);
	va_end(args);
	assert((flags & O_TMPFILE) == O_TMPFILE);
	if (create_error) {
		errno = create_error;
		return -1;
	}
	return __real_openat64(fd, path, flags, mode);
}

int __real_fstatfs64(int fd, struct statfs64 *buf);
int __wrap_fstatfs64(int fd, struct statfs64 *buf)
{
	int ret = __real_fstatfs64(fd, buf);
	if (!ret && filesystem_type)
		buf->f_type = filesystem_type;
	return ret;
}

void *__real_calloc(size_t count, size_t bytes);
void *__wrap_calloc(size_t count, size_t bytes)
{
	return heap_error ? NULL : __real_calloc(count, bytes);
}

void *__real_malloc(size_t bytes);
void *__wrap_malloc(size_t bytes)
{
	return malloc_error ? NULL : __real_malloc(bytes);
}

void *__real_mmap64(void *addr, size_t bytes, int prot, int flags, int fd, off64_t offset);
void *__wrap_mmap64(void *addr, size_t bytes, int prot, int flags, int fd, off64_t offset)
{
	struct stat st;
	assert(fd == reserved_fd);
	assert(flags == MAP_SHARED && prot == (PROT_READ | PROT_WRITE));
	assert(!offset && !fstat(fd, &st) && st.st_nlink == 0);
	assert(st.st_size == (off_t)bytes && (fcntl(fd, F_GETFD) & FD_CLOEXEC));
	reserved_fd = -1;
	if (map_error) {
		errno = map_error;
		return MAP_FAILED;
	}
	void *p = __real_mmap64(addr, bytes, prot, flags, fd, offset);
	assert(p != MAP_FAILED);
	for (unsigned int i = 0; i < 128; ++i) {
		if (!mappings[i].p) {
			mappings[i].p = p;
			mappings[i].bytes = bytes;
			++live_maps;
			return p;
		}
	}
	abort();
}

int __real_munmap(void *addr, size_t bytes);
int __wrap_munmap(void *addr, size_t bytes)
{
	for (unsigned int i = 0; i < 128; ++i) {
		if (mappings[i].p == addr) {
			assert(mappings[i].bytes == bytes);
			mappings[i].p = NULL;
			--live_maps;
			return __real_munmap(addr, bytes);
		}
	}
	abort();
}

static int fd_count(void)
{
	DIR *dir = opendir("/proc/self/fd");
	int count = 0;
	assert(dir);
	while (readdir(dir))
		++count;
	closedir(dir);
	return count;
}

static void check_clean(int fds)
{
	erofs_blob_exit();
	erofs_blob_exit();
	assert(!live_maps && fd_count() == fds);
	assert(blobfile == -1 && blob_index_fd == -1);
	assert(!blob_hashmap.table && !blob_record_segments);
	assert(list_empty(&unhashed_blobchunks));
}

static void check_chains(struct hashmap *reference)
{
	assert(reference->tablesize == blob_hashmap.tablesize);
	assert(reference->size == blob_hashmap.size);
	for (unsigned int i = 0; i < reference->tablesize; ++i) {
		struct hashmap_entry *a = reference->table[i], *b = blob_hashmap.table[i];
		for (; a && b; a = a->next, b = b->next) {
			struct erofs_blobchunk *ca = (void *)a, *cb = (void *)b;
			assert(!memcmp(ca->sha256, cb->sha256, sizeof(ca->sha256)));
		}
		assert(!a && !b);
	}
}

int main(int argc, char **argv)
{
	assert(argc == 2);
	assert(!setenv("TMPDIR", argv[1], 1));
	assert(!setenv("EROFS_INDEX_STORAGE", "disk", 1));
	int fds = fd_count();

	/* Init failure, unsupported capabilities, and reinitialization unwind. */
	for (int n = 0; n < 5; ++n) {
		int errors[] = {ENOSPC, EDQUOT, EOPNOTSUPP, ENOMEM, EACCES};
		reserve_error = n < 3 ? errors[n] : 0;
		map_error = n == 3 ? errors[n] : 0;
		create_error = n == 4 ? errors[n] : 0;
		assert(erofs_blob_init(NULL, 4096) == -errors[n]);
		check_clean(fds);
	}
	reserve_error = map_error = create_error = 0;
	heap_error = 1;
	assert(erofs_blob_init(NULL, 4096) == -ENOMEM); /* zero-chunk buffer */
	heap_error = 0;
	check_clean(fds);
	for (int i = 0; i < 2; ++i) {
		filesystem_type = i ? RAMFS_MAGIC : TMPFS_MAGIC;
		assert(erofs_blob_init_storage() == -EOPNOTSUPP);
		check_clean(fds);
	}
	filesystem_type = 0;
	assert(erofs_blob_init("/no-such-index-test-directory/blob", 4096) == -ENOENT);
	check_clean(fds);
	assert(!erofs_blob_init_storage());
	malloc_error = 1;
	assert(PTR_ERR(blob_alloc_chunk(false)) == -ENOMEM); /* arena metadata */
	malloc_error = 0;
	assert(PTR_ERR(erofs_index_mmap(blob_index_fd, SIZE_MAX)) == -EOVERFLOW);
	assert(PTR_ERR(erofs_index_mmap(blob_index_fd, 0)) == -EOVERFLOW);
	/* Failure allocating the initial arena must release the directory. */
	reserve_error = ENOSPC;
	assert(PTR_ERR(blob_alloc_chunk(false)) == -ENOSPC);
	reserve_error = 0;
	check_clean(fds);

	/* Failed growth leaves buckets, links, size and entry ownership intact. */
	assert(!erofs_blob_init(NULL, 4096));
	assert(erofs_blob_init(NULL, 4096) == -EBUSY);
	for (unsigned int i = 1; i < 51; ++i) {
		struct erofs_blobchunk *c = blob_alloc_chunk(true);
		assert(!IS_ERR(c));
		hashmap_entry_init(c, i);
		assert(!blob_hashmap_add(c));
	}
	struct hashmap saved = blob_hashmap;
	struct hashmap_entry *buckets[64];
	memcpy(buckets, saved.table, sizeof(buckets));
	struct erofs_blobchunk *c = blob_alloc_chunk(true);
	hashmap_entry_init(c, 53);
	for (int i = 0; i < 2; ++i) {
		reserve_error = i ? 0 : ENOSPC;
		map_error = i ? ENOMEM : 0;
		assert(blob_hashmap_add(c) == -(i ? ENOMEM : ENOSPC));
		assert(!memcmp(&saved, &blob_hashmap, sizeof(saved)));
		assert(!memcmp(buckets, saved.table, sizeof(buckets)) && !c->ent.next);
		assert(live_maps == 2);
	}
	reserve_error = map_error = 0;
	assert(!blob_hashmap_add(c));
	assert(live_maps == 2); /* The retired bucket generation was unmapped. */
	saved = blob_hashmap;
	blob_hashmap.size = UINT_MAX;
	assert(blob_hashmap_add(c) == -EOVERFLOW);
	blob_hashmap.size = blob_hashmap.grow_at;
	blob_hashmap.tablesize = UINT_MAX / 4 + 1;
	assert(blob_hashmap_add(c) == -EOVERFLOW);
	blob_hashmap = saved;
	check_clean(fds);

	/* Three arenas (<13 MiB), stable live pointers, and exact upstream hash
	 * chain order even through collisions and several table generations.
	 */
	assert(!erofs_blob_init(NULL, 4096));
	unsigned int count = 2 * (BLOB_RECORD_SEGMENT_SIZE / sizeof(*c)) + 7;
	struct erofs_blobchunk *refs = calloc(count + 1, sizeof(*refs));
	assert(refs);
	struct hashmap reference;
	hashmap_init(&reference, erofs_blob_hashmap_cmp, 0);
	struct hashmap_iter iter;
	refs[0] = *(struct erofs_blobchunk *)hashmap_iter_first(&blob_hashmap, &iter);
	hashmap_add(&reference, refs);
	struct erofs_blobchunk *first = NULL;
	for (unsigned int i = 1; i <= count; ++i) {
		c = blob_alloc_chunk(true);
		assert(!IS_ERR(c));
		memcpy(c->sha256, &i, sizeof(i));
		hashmap_entry_init(c, i % 257);
		refs[i] = *c;
		assert(!blob_hashmap_add(c));
		hashmap_add(&reference, refs + i);
		if (i == 1)
			first = c;
	}
	check_chains(&reference);
	assert(hashmap_get_from_hash(&blob_hashmap, 1, refs[1].sha256) == first);
	assert(live_maps == 4 && fd_count() == fds + 2); /* dir + blob only */
	free(reference.table);
	free(refs);
	check_clean(fds);

	/* Unhashed record arena lifetime and original array lengths after merge. */
	assert(!erofs_blob_init_storage());
	assert(!IS_ERR(erofs_get_unhashed_chunk(1, 10, 0)));
	struct erofs_sb_info sbi = {.blkszbits = 12};
	struct erofs_inode inode = {.sbi = &sbi, .i_size = 8 * 1024 * 1024 + 1};
	reserve_error = ENOSPC;
	assert(erofs_blob_init_chunkindexes(&inode, 12, 4) == -ENOSPC);
	assert(!inode.chunkindexes && !inode.chunkindexes_mmap_size);
	reserve_error = 0;
	assert(!erofs_blob_init_chunkindexes(&inode, 12, 4));
	size_t original = inode.chunkindexes_mmap_size;
	assert(original > BLOB_INDEX_MMAP_MIN);
	for (unsigned int i = 0; i < original / sizeof(void *); ++i)
		((void **)inode.chunkindexes)[i] = &erofs_holechunk;
	assert(!erofs_blob_mergechunks(&inode, 12, 14));
	assert(inode.extent_isize < original && inode.chunkindexes_mmap_size == original);
	erofs_blob_exit(); /* Array release must not depend on the open directory. */
	erofs_blob_free_chunkindexes(&inode);
	erofs_blob_free_chunkindexes(&inode);
	check_clean(fds);

	assert(!erofs_blob_init_storage());
	unsigned int calls = reserve_calls;
	for (unsigned int i = 0; i < 1000; ++i) {
		inode.i_size = i;
		assert(!erofs_blob_init_chunkindexes(&inode, 12, 4));
		assert(!inode.chunkindexes_mmap_size);
		erofs_blob_free_chunkindexes(&inode);
	}
	assert(reserve_calls == calls);
	inode.i_size = UINT64_MAX;
	assert(erofs_blob_init_chunkindexes(&inode, 12, 8) == -EOVERFLOW);
	assert(erofs_blob_init_chunkindexes(&inode, 64, 8) == -EOVERFLOW);
	check_clean(fds);

	/* Memory mode ignores scratch, with checked heap failures and no mmap. */
	assert(!setenv("EROFS_INDEX_STORAGE", "memory", 1));
	assert(!erofs_blob_init_storage());
	heap_error = 1;
	assert(PTR_ERR(blob_alloc_buckets(64)) == -ENOMEM);
	inode.i_size = 4096;
	assert(erofs_blob_init_chunkindexes(&inode, 12, 4) == -ENOMEM);
	heap_error = 0;
	check_clean(fds);
	puts("erofs-index-unit: reservation, rollback, arenas, chains, lengths, overflow, cleanup PASS");
	return 0;
}
