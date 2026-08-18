# Neutral note: WebAssembly code generation in SpiderMonkey

This note provides general background on the WebAssembly implementation
areas in SpiderMonkey. It does not identify the exact function, file, or
trigger sequence of any defect.

## Background

SpiderMonkey's WebAssembly support lives under `js/src/wasm/`. The engine
compiles WebAssembly modules through a tiered pipeline: a fast baseline
compiler and an optimizing Cranelift/Ion compiler. Compiled functions are
held in `Code` / `CodeBlock` objects that manage executable memory
allocations.

The `wasm/` subdirectory covers several broad areas:

- **Module compilation** (`WasmGenerator.cpp`, `WasmCompile.cpp`) —
  validating and compiling function bodies
- **Code management** (`WasmCode.cpp`, `WasmCode.h`) — managing
  executable code segments, code allocations, and metadata
- **Instance lifecycle** (`WasmInstance.cpp`) — creating module instances,
  exporting functions, and servicing calls into wasm from JS
- **Tables and globals** (`WasmTable.cpp`, `WasmGlobals.cpp`) — managing
  WebAssembly tables, element segments, and global variables
- **Memory** (`WasmMemory.cpp`) — linear memory allocation and bounds
  management

Executable code is allocated from a process-wide pool of mapped virtual
memory pages. Functions compiled eagerly at module-instantiation time, as
well as code generated lazily at runtime, share this pool.

## How to investigate

When auditing this codebase for memory-safety issues, useful approaches
include:

- Review all code paths that allocate, claim, or manage executable memory
  segments for bounds-correctness
- Check that buffer operations (copy, zero-fill) are anchored at the
  correct base address and length for their target allocation
- Trace how exported wasm functions are dispatched when called from JS,
  including any code paths that generate or look up interpreter entries
- Examine the interaction between the module's exported function table,
  element segments, and the call dispatch path

The source tree is available at `/src/firefox/js/src/`.
