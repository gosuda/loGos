// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && unix

#include <errno.h>
#include <pthread.h>
#include <stdint.h>
#include <string.h>
#include <time.h>

#include "_cgo_export.h"

// Deterministic barriers for the foreign-root attacks. Each gate is one
// mutex and one condition variable: a holder announces arrival and waits
// for release on a predicate, the Go controller waits for arrival and
// releases. No sleeps; the bounded waits only turn a deadlock into a failed
// status. Every gate field that two threads touch is read and written under
// the gate mutex; the only unlocked cross-thread reads are of Go referent
// memory, which Go itself never writes while a holder owns it, so any
// change C sees there is the defect under test.

#define FR_STAMP 0x5A5A5A5A5A5A5A5AULL
#define FR_WAIT_SECONDS 120

enum {
	FR_NIL = 1,
	FR_ENTRY = 2,
	FR_HELD = 3,
	FR_BADARG = 4,
	FR_TIMEOUT = 5,
	FR_WORD = 6
};

// Status word: (index+1)<<4 | code, 0 on success. Go decodes it in
// frStatus.
#define FR_ST(i, code) ((((i) + 1) << 4) | (code))

typedef struct fr_gate {
	pthread_mutex_t mu;
	pthread_cond_t cv;
	int idx;
	int entered;
	int released;
	uintptr_t addr[FR_MAX_REFS];
	void *kept[FR_MAX_KEPT];
	unsigned int tag[FR_MAX_KEPT];
	unsigned int words[FR_MAX_KEPT];
	unsigned int skip[FR_MAX_KEPT];
	uint64_t kept_stamp;
	pthread_t th;
	int th_n;
	int th_stamp;
	int th_unpinned;
	int th_status;
} fr_gate;

static fr_gate fr_gates[FR_MAX_GATES];

// Waits are measured on the monotonic clock wherever condition variables
// can be bound to it, so a wall-clock step never shortens or stretches the
// bound.
#if defined(__linux__)
#define FR_WAIT_CLOCK CLOCK_MONOTONIC
#else
#define FR_WAIT_CLOCK CLOCK_REALTIME
#endif

static void
fr_deadline(struct timespec *ts)
{
	clock_gettime(FR_WAIT_CLOCK, ts);
	ts->tv_sec += FR_WAIT_SECONDS;
}

static fr_gate *
fr_gate_at(int gi)
{
	return &fr_gates[((unsigned int)gi) % FR_MAX_GATES];
}

// Every gate's mutex and condition variable is initialized exactly once, for
// the life of the process; a gate is recycled by resetting its fields under
// its own mutex, never by re-initializing the pthread objects.
static pthread_once_t fr_once = PTHREAD_ONCE_INIT;

static void
fr_init_gates(void)
{
	int i;

	for (i = 0; i < FR_MAX_GATES; i++) {
		fr_gate *g = &fr_gates[i];
		pthread_condattr_t attr;

		g->idx = i;
		pthread_mutex_init(&g->mu, NULL);
		pthread_condattr_init(&attr);
#if defined(__linux__)
		pthread_condattr_setclock(&attr, CLOCK_MONOTONIC);
#endif
		pthread_cond_init(&g->cv, &attr);
		pthread_condattr_destroy(&attr);
	}
}

void
fr_gate_reset(int gi)
{
	fr_gate *g;

	pthread_once(&fr_once, fr_init_gates);
	g = fr_gate_at(gi);
	pthread_mutex_lock(&g->mu);
	g->entered = 0;
	g->released = 0;
	memset(g->addr, 0, sizeof g->addr);
	memset(g->kept, 0, sizeof g->kept);
	memset(g->tag, 0, sizeof g->tag);
	memset(g->words, 0, sizeof g->words);
	memset(g->skip, 0, sizeof g->skip);
	g->kept_stamp = 0;
	g->th_n = 0;
	g->th_stamp = 0;
	g->th_unpinned = 0;
	g->th_status = 0;
	pthread_mutex_unlock(&g->mu);
}

void
fr_gate_release(int gi)
{
	fr_gate *g = fr_gate_at(gi);

	pthread_mutex_lock(&g->mu);
	g->released = 1;
	pthread_cond_broadcast(&g->cv);
	pthread_mutex_unlock(&g->mu);
}

int
fr_gate_wait_entered(int gi, int want)
{
	fr_gate *g = fr_gate_at(gi);
	struct timespec dl;
	int rc = 0;
	int ok;

	fr_deadline(&dl);
	pthread_mutex_lock(&g->mu);
	while (g->entered < want && rc == 0)
		rc = pthread_cond_timedwait(&g->cv, &g->mu, &dl);
	ok = g->entered >= want;
	pthread_mutex_unlock(&g->mu);
	return ok ? 0 : 1;
}

// fr_arrive announces arrival and waits for release. It returns 0, or 1 on
// the bounded timeout.
static int
fr_arrive(fr_gate *g)
{
	struct timespec dl;
	int rc = 0;
	int ok;

	fr_deadline(&dl);
	pthread_mutex_lock(&g->mu);
	g->entered++;
	pthread_cond_broadcast(&g->cv);
	while (!g->released && rc == 0)
		rc = pthread_cond_timedwait(&g->cv, &g->mu, &dl);
	ok = g->released;
	pthread_mutex_unlock(&g->mu);
	return ok ? 0 : 1;
}

void
fr_meta_set(int gi, int i, unsigned int tag, unsigned int words, unsigned int skip)
{
	fr_gate *g = fr_gate_at(gi);

	pthread_mutex_lock(&g->mu);
	if (i >= 0 && i < FR_MAX_KEPT) {
		g->tag[i] = tag;
		g->words[i] = words;
		g->skip[i] = skip;
	}
	pthread_mutex_unlock(&g->mu);
}

static void
fr_meta_get(fr_gate *g, int i, unsigned int *tag, unsigned int *words, unsigned int *skip)
{
	pthread_mutex_lock(&g->mu);
	*tag = g->tag[i];
	*words = g->words[i];
	*skip = g->skip[i];
	pthread_mutex_unlock(&g->mu);
}

// fr_note records the pointer C received in slot i.
static void
fr_note(fr_gate *g, int i, void *p)
{
	pthread_mutex_lock(&g->mu);
	g->addr[i] = (uintptr_t)p;
	pthread_mutex_unlock(&g->mu);
}

// fr_note_kept records the pointer in slot i and keeps it for a later call.
static void
fr_note_kept(fr_gate *g, int i, void *p)
{
	pthread_mutex_lock(&g->mu);
	g->addr[i] = (uintptr_t)p;
	g->kept[i] = p;
	pthread_mutex_unlock(&g->mu);
}

static void *
fr_kept_get(fr_gate *g, int i)
{
	void *p;

	pthread_mutex_lock(&g->mu);
	p = g->kept[i];
	pthread_mutex_unlock(&g->mu);
	return p;
}

uintptr_t
fr_entry_addr(int gi, int i)
{
	fr_gate *g = fr_gate_at(gi);
	uintptr_t a = 0;

	pthread_mutex_lock(&g->mu);
	if (i >= 0 && i < FR_MAX_REFS)
		a = g->addr[i];
	pthread_mutex_unlock(&g->mu);
	return a;
}

uintptr_t
fr_addr(void *p)
{
	return (uintptr_t)p;
}

// The data contract mirrors frWord in foreign_roots_test.go (splitmix64).
static uint64_t
fr_mix(uint64_t x)
{
	x += 0x9E3779B97F4A7C15ULL;
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9ULL;
	x = (x ^ (x >> 27)) * 0x94D049BB133111EBULL;
	return x ^ (x >> 31);
}

static uint64_t
fr_word(unsigned int tag, uint64_t j)
{
	return fr_mix((uint64_t)tag * 0x9E3779B97F4A7C15ULL + j + 1);
}

// fr_verify checks words uint64 values at p, which sits skip words into an
// object whose word j holds fr_word(tag, j), except that the last word of
// the window must equal its contract word XOR wantstamp: holds that have
// not stamped pass 0, and fr_kept_check passes the stamp its gate's holder
// actually applied. The mutex operations around the barrier are the
// compiler barriers that force the check after the barrier to read the
// memory again.
static int
fr_verify(const void *p, unsigned int tag, unsigned int words, unsigned int skip, uint64_t wantstamp)
{
	const uint64_t *w = (const uint64_t *)p;
	unsigned int m;

	for (m = 0; m < words; m++) {
		uint64_t want = fr_word(tag, (uint64_t)skip + m);

		if (m == words - 1)
			want ^= wantstamp;
		if (w[m] != want)
			return 0;
	}
	return 1;
}

// fr_stamp XORs the stamp into the last word of the window at p.
static void
fr_stamp(void *p, unsigned int words)
{
	uint64_t *w = (uint64_t *)p;

	if (words > 0)
		w[words - 1] ^= FR_STAMP;
}

int
fr_hold_refs(int gi, fr_refs refs, int stamp)
{
	fr_gate *g = fr_gate_at(gi);
	int n = refs.n;
	int status = 0;
	int i;

	if (n < 0 || n > FR_MAX_REFS) {
		status = FR_ST(0, FR_BADARG);
		n = 0;
	}
	for (i = 0; i < n; i++) {
		fr_note(g, i, refs.r[i].p);
		if (status != 0)
			continue;
		if (refs.r[i].p == NULL)
			status = FR_ST(i, FR_NIL);
		else if (!fr_verify(refs.r[i].p, refs.r[i].tag, refs.r[i].words, refs.r[i].skip, 0))
			status = FR_ST(i, FR_ENTRY);
	}
	if (fr_arrive(g) != 0 && status == 0)
		status = FR_ST(0, FR_TIMEOUT);
	for (i = 0; i < n && status == 0; i++) {
		if (!fr_verify(refs.r[i].p, refs.r[i].tag, refs.r[i].words, refs.r[i].skip, 0))
			status = FR_ST(i, FR_HELD);
	}
	if (stamp && status == 0) {
		for (i = 0; i < n; i++)
			fr_stamp(refs.r[i].p, refs.r[i].words);
	}
	return status;
}

// fr_try_refs holds refs only as the call's own arguments; the callback
// result check is what runs. FrUnpinned returns an unpinned Go pointer, so
// the generated check must panic and unwind this frame. A normal return
// means the result was accepted.
int
fr_try_refs(fr_refs refs)
{
	void *r;

	(void)refs;
	r = FrUnpinned();
	return r != NULL ? 0 : 1;
}

// fr_cb_body is the zero-pointer-input holder: every pointer comes from a
// callback and is kept in the gate, as cgo allows for pinned pointers.
static int
fr_cb_body(fr_gate *g, int n, int stamp)
{
	unsigned int tag, words, skip;
	int status = 0;
	int i;

	if (n < 0 || n > FR_MAX_KEPT) {
		status = FR_ST(0, FR_BADARG);
		n = 0;
	}
	for (i = 0; i < n; i++) {
		void *p = FrProvide(g->idx, i);

		fr_note_kept(g, i, p);
		if (status != 0)
			continue;
		fr_meta_get(g, i, &tag, &words, &skip);
		if (p == NULL)
			status = FR_ST(i, FR_NIL);
		else if (!fr_verify(p, tag, words, skip, 0))
			status = FR_ST(i, FR_ENTRY);
	}
	if (fr_arrive(g) != 0 && status == 0)
		status = FR_ST(0, FR_TIMEOUT);
	for (i = 0; i < n && status == 0; i++) {
		fr_meta_get(g, i, &tag, &words, &skip);
		if (!fr_verify(fr_kept_get(g, i), tag, words, skip, 0))
			status = FR_ST(i, FR_HELD);
	}
	if (stamp && status == 0) {
		for (i = 0; i < n; i++) {
			fr_meta_get(g, i, &tag, &words, &skip);
			fr_stamp(fr_kept_get(g, i), words);
		}
		pthread_mutex_lock(&g->mu);
		g->kept_stamp = FR_STAMP;
		pthread_mutex_unlock(&g->mu);
	}
	return status;
}

int
fr_cb_hold(int gi, int n, int stamp)
{
	return fr_cb_body(fr_gate_at(gi), n, stamp);
}

int
fr_kept_check(int gi, int from, int to)
{
	fr_gate *g = fr_gate_at(gi);
	unsigned int tag, words, skip;
	uint64_t wantstamp;
	void *p;
	int i;

	if (from < 0 || to > FR_MAX_KEPT || from > to)
		return FR_ST(0, FR_BADARG);
	pthread_mutex_lock(&g->mu);
	wantstamp = g->kept_stamp;
	pthread_mutex_unlock(&g->mu);
	for (i = from; i < to; i++) {
		p = fr_kept_get(g, i);
		if (p == NULL)
			return FR_ST(i, FR_NIL);
		fr_meta_get(g, i, &tag, &words, &skip);
		if (!fr_verify(p, tag, words, skip, wantstamp))
			return FR_ST(i, FR_HELD);
	}
	return 0;
}

// The thread fields are written before pthread_create and read by the
// thread after it starts; th_status is written by the thread and read after
// pthread_join. Create and join are the synchronization.
static void *
fr_thread_main(void *arg)
{
	fr_gate *g = (fr_gate *)arg;

	if (g->th_unpinned) {
		void *r = FrUnpinned();

		g->th_status = r != NULL ? 0 : 1;
	} else {
		g->th_status = fr_cb_body(g, g->th_n, g->th_stamp);
	}
	return NULL;
}

int
fr_thread_start(int gi, int n, int stamp, int unpinned)
{
	fr_gate *g = fr_gate_at(gi);

	g->th_n = n;
	g->th_stamp = stamp;
	g->th_unpinned = unpinned;
	g->th_status = 0;
	return pthread_create(&g->th, NULL, fr_thread_main, g);
}

int
fr_thread_join(int gi)
{
	fr_gate *g = fr_gate_at(gi);

	pthread_join(g->th, NULL);
	return g->th_status;
}

typedef struct fr_node {
	void *next;
	unsigned int tag;
	unsigned int words;
} fr_node;

// fr_hold_node follows the nested pointer of a top-level node. Both the
// node and its nested word must stay exactly as C first saw them while C
// waits at the barrier: C-visible memory is never rewritten after entry.
int
fr_hold_node(int gi, void *np)
{
	fr_gate *g = fr_gate_at(gi);
	fr_node *n = (fr_node *)np;
	void *leaf = n->next;
	unsigned int words = n->words;
	unsigned int tag = n->tag;
	int status = 0;

	fr_note(g, 0, n);
	fr_note(g, 1, leaf);
	if (leaf == NULL)
		status = FR_ST(1, FR_NIL);
	else if (!fr_verify(leaf, tag, words, 0, 0))
		status = FR_ST(1, FR_ENTRY);
	if (fr_arrive(g) != 0 && status == 0)
		status = FR_ST(0, FR_TIMEOUT);
	if (status == 0 && n->next != leaf)
		status = FR_ST(1, FR_WORD);
	if (status == 0 && !fr_verify(leaf, tag, words, 0, 0))
		status = FR_ST(1, FR_HELD);
	if (status == 0)
		fr_stamp(leaf, words);
	return status;
}

typedef struct fr_idx_elem {
	void *p;
	uint64_t v;
} fr_idx_elem;

// fr_hold_indexed holds the fixed-array tile handed as the interior pointer
// &(*pa)[start]: cgo roots pa itself and indexes through it, so C points
// into the caller's array.
// C derives the tile base as (fr_idx_elem *)p - start and verifies every
// element's pointer leaf (fr_verify against tag0+k) and scalar word
// (== fr_word(tagV0, k)), including the five pointer cells after the first,
// waits at the barrier while Go forces moving collections, re-verifies, and
// stamps leaves plus array V words. Entry mismatches report ENTRY, held
// mismatches HELD; a null leaf reports NIL. Stamping the array V words proves
// C and Go name the same tile: stamps on a detached boxed copy would never
// reach the caller's array.
int
fr_hold_indexed(int gi, void *p, int start, int n, unsigned int tag0, unsigned int tagV0, int stamp)
{
	fr_gate *g = fr_gate_at(gi);
	fr_idx_elem *elem = (fr_idx_elem *)p;
	fr_idx_elem *base;
	int status = 0;
	int k;

	if (n != 8 || start < 0 || start >= n || p == NULL) {
		return FR_ST(0, FR_BADARG);
	}
	base = elem - start;
	fr_note(g, 0, p);
	for (k = 0; k < n; k++) {
		void *leaf = base[k].p;
		if (leaf == NULL) {
			status = FR_ST(k, FR_NIL);
			break;
		}
		if (!fr_verify(leaf, tag0 + (unsigned int)k, 8, 0, 0)) {
			status = FR_ST(k, FR_ENTRY);
			break;
		}
		if (base[k].v != fr_word(tagV0, (uint64_t)k)) {
			status = FR_ST(k, FR_ENTRY);
			break;
		}
	}
	if (fr_arrive(g) != 0 && status == 0)
		status = FR_ST(0, FR_TIMEOUT);
	for (k = 0; k < n && status == 0; k++) {
		void *leaf = base[k].p;
		if (!fr_verify(leaf, tag0 + (unsigned int)k, 8, 0, 0)) {
			status = FR_ST(k, FR_HELD);
			break;
		}
		if (base[k].v != fr_word(tagV0, (uint64_t)k)) {
			status = FR_ST(k, FR_HELD);
			break;
		}
	}
	if (stamp && status == 0) {
		for (k = 0; k < n; k++) {
			fr_stamp(base[k].p, 8);
			base[k].v ^= FR_STAMP;
		}
	}
	return status;
}
