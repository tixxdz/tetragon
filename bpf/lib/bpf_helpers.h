// SPDX-License-Identifier: GPL-2.0
/* Copyright Authors of Cilium */

#ifndef __BPF_HELPERS_
#define __BPF_HELPERS_

#include "api.h"

#ifndef PATH_MAP_SIZE
#define PATH_MAP_SIZE 4096
#endif

#ifndef __READ_ONCE
#define __READ_ONCE(x) (*(volatile typeof(x) *)&x)
#endif
#ifndef __WRITE_ONCE
#define __WRITE_ONCE(x, v) (*(volatile typeof(x) *)&x) = (v)
#endif

#ifndef READ_ONCE
#define READ_ONCE(x)                                                           \
	({                                                                     \
		typeof(x) __val;                                               \
		__val = __READ_ONCE(x);                                        \
		compiler_barrier();                                            \
		__val;                                                         \
	})
#endif
#ifndef WRITE_ONCE
#define WRITE_ONCE(x, v)                                                       \
	({                                                                     \
		typeof(x) __val = (v);                                         \
		__WRITE_ONCE(x, __val);                                        \
		compiler_barrier();                                            \
		__val;                                                         \
	})
#endif

#define XSTR(s) STR(s)
#define STR(s)	#s

/*
 * Following define is to assist VSCode Intellisense so that it treats
 * __builtin_preserve_access_index() as a const void * instead of a
 * simple void (because it doesn't have a definition for it). This stops
 * Intellisense marking all _(P) macros (used in probe_read()) as errors.
 * To use this, just define VSCODE in 'C/C++: Edit Configurations (JSON)'
 * in the Command Palette in VSCODE (F1 or View->Command Palette...):
 *    "defines": ["VSCODE"]
 * under configurations.
 */
#ifdef VSCODE
const void *__builtin_preserve_access_index(void *);
#endif
#define _(P) (__builtin_preserve_access_index(P))

/*
 * Convenience macro to check that field actually exists in target kernel's.
 * Returns:
 *    1, if matching field is present in target kernel;
 *    0, if no matching field found.
 */
#define bpf_core_field_exists(field)                                           \
	__builtin_preserve_field_info(field, BPF_FIELD_EXISTS)

/* second argument to __builtin_preserve_enum_value() built-in */
enum bpf_enum_value_kind {
	BPF_ENUMVAL_EXISTS = 0, /* enum value existence in kernel */
	BPF_ENUMVAL_VALUE = 1, /* enum value value relocation */
};

/*
 * bpf_probe_read
 *
 * For tracing programs, safely attempt to read *size* bytes from
 * kernel space address *unsafe_ptr* and store the data in *dst*.
 *
 * Generally, use **bpf_probe_read_user**\ () or
 * **bpf_probe_read_kernel**\ () instead.
 *
 * Returns
 * 0 on success, or a negative error in case of failure.
 */
static long (*bpf_probe_read)(void *dst, __u32 size,
			      const void *unsafe_ptr) = (void *)4;

/*
 * Convenience macro to get the integer value of an enumerator value in
 * a target kernel.
 * Returns:
 *    64-bit value, if specified enum type and its enumerator value are
 *    present in target kernel's BTF;
 *    0, if no matching enum and/or enum value within that enum is found.
 */
#define bpf_core_enum_value(enum_type, enum_value)                             \
	__builtin_preserve_enum_value(*(typeof(enum_type) *)enum_value,        \
				      BPF_ENUMVAL_VALUE)

#include "bpf_core_read.h"

/* relax_verifier is a dummy helper call to introduce a pruning checkpoint
 * to help relax the verifier to avoid reaching complexity limits.
 */
static inline __attribute__((always_inline)) void relax_verifier(void)
{
	/* Calling get_smp_processor_id() in asm saves an instruction as we
	 * don't have to store the result to ensure the call takes place.
	 * However, we have to specifiy the call target by number and not
	 * name, hence 'call 8'. This is unlikely to change, though, so this
	 * isn't a big issue.
	 */
	asm volatile("call 8;\n" ::: "r0", "r1", "r2", "r3", "r4", "r5");
}

static inline void compiler_barrier(void)
{
	asm volatile("" ::: "memory");
}
#endif //__BPF_HELPERS_
