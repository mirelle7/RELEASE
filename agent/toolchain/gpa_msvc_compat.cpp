// The few Microsoft C/C++ runtime helpers that Microsoft's own d3dx8.lib (from TheSuperHackers/min-dx8-sdk, the
// static D3DX the VC6 and Visual Studio builds link) needs, so that the MinGW-w64 builds can link that same
// library instead of the debug import library (libd3dx8d.a) that would need d3dx8d.dll at run time.
// Built and added to the library by the agent; the game's source is not touched.
#include <new>
#include <cstddef>

typedef void(__attribute__((thiscall)) * gpa_ctor_t)(void *);

// The MSVC-mangled names contain characters ("?", "@") that must be quoted in assembly: clang quotes an asm label by
// itself, gcc writes it out as given.
#ifdef __clang__
#define GPA_SYM(s) s
#else
#define GPA_SYM(s) "\"" s "\""
#endif

extern "C" {
// "compiler uses floating point" marker the MSVC linker looks for
int _fltused = 0x9875;
}

// MSVC-mangled operator new / delete
void *__attribute__((cdecl)) gpa_op_new(unsigned n) __asm__(GPA_SYM("??2@YAPAXI@Z"));
void *__attribute__((cdecl)) gpa_op_new(unsigned n) { return ::operator new(static_cast<std::size_t>(n)); }
void __attribute__((cdecl)) gpa_op_delete(void *p) __asm__(GPA_SYM("??3@YAXPAX@Z"));
void __attribute__((cdecl)) gpa_op_delete(void *p) { ::operator delete(p); }

// `eh vector constructor iterator' and `eh vector destructor iterator': run a constructor / destructor over an array.
// (The original also unwinds a half-built array if a constructor throws; D3DX's constructors do not.)
void __attribute__((stdcall)) gpa_vec_ctor(void *p, unsigned size, int count, gpa_ctor_t ctor, gpa_ctor_t dtor) __asm__(GPA_SYM("??_L@YGXPAXIHP6EX0@Z1@Z"));
void __attribute__((stdcall)) gpa_vec_ctor(void *p, unsigned size, int count, gpa_ctor_t ctor, gpa_ctor_t dtor) {
  char *c = static_cast<char *>(p);
  for (int i = 0; i < count; ++i) ctor(c + static_cast<std::size_t>(i) * size);
  (void)dtor;
}
void __attribute__((stdcall)) gpa_vec_dtor(void *p, unsigned size, int count, gpa_ctor_t dtor) __asm__(GPA_SYM("??_M@YGXPAXIHP6EX0@Z@Z"));
void __attribute__((stdcall)) gpa_vec_dtor(void *p, unsigned size, int count, gpa_ctor_t dtor) {
  char *c = static_cast<char *>(p);
  for (int i = count - 1; i >= 0; --i) dtor(c + static_cast<std::size_t>(i) * size);
}

__asm__(
    ".text\n"
    // fs:[__except_list] is the head of the SEH chain: offset 0 of the thread block
    ".globl __except_list\n"
    ".set __except_list, 0\n"
    // unsigned 64-bit shift right: edx:eax >>= cl
    ".globl __aullshr\n"
    "__aullshr:\n"
    "  cmpb $64, %cl\n"
    "  jae 2f\n"
    "  cmpb $32, %cl\n"
    "  jae 1f\n"
    "  shrdl %cl, %edx, %eax\n"
    "  shrl %cl, %edx\n"
    "  ret\n"
    "1:\n"
    "  movl %edx, %eax\n"
    "  xorl %edx, %edx\n"
    "  andb $31, %cl\n"
    "  shrl %cl, %eax\n"
    "  ret\n"
    "2:\n"
    "  xorl %eax, %eax\n"
    "  xorl %edx, %edx\n"
    "  ret\n"
    // stack probe: eax = bytes wanted; touches each page, then lowers esp by eax (the Microsoft __chkstk contract)
    ".globl __alloca_probe\n"
    "__alloca_probe:\n"
    "  pushl %ecx\n"
    "  leal 8(%esp), %ecx\n"
    "  subl %eax, %ecx\n"
    "  sbbl %eax, %eax\n"
    "  notl %eax\n"
    "  andl %eax, %ecx\n"
    "  movl %esp, %eax\n"
    "  andl $0xFFFFF000, %eax\n"
    "3:\n"
    "  cmpl %eax, %ecx\n"
    "  jb 4f\n"
    "  movl %ecx, %eax\n"
    "  popl %ecx\n"
    "  xchgl %eax, %esp\n"
    "  movl (%eax), %eax\n"
    "  movl %eax, (%esp)\n"
    "  ret\n"
    "4:\n"
    "  subl $0x1000, %eax\n"
    "  testl %eax, (%eax)\n"
    "  jmp 3b\n");
