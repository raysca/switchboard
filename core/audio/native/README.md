# Vendored SpeexDSP subset

The C sources and headers in this directory and its parent `core/audio/`
come from the official Xiph SpeexDSP release `SpeexDSP-1.2.1`, commit
`1b28a0f61bc31162979e1f26f3981fc3637095c8`:

- <https://gitlab.xiph.org/xiph/speexdsp>
- <https://github.com/xiph/speexdsp/releases/tag/SpeexDSP-1.2.1>

The vendored `.c` files come from upstream `libspeexdsp/`; the headers under
`native/libspeexdsp/` and `native/speex/` come from upstream `libspeexdsp/`
and `include/speex/`. A clean-checkout audit found them byte-for-byte identical
before three diff-hygiene-only normalizations:

- the final blank line was removed from `kiss_fft.c`;
- the final blank line was removed from `preprocess.c`, and two mixed
  space/tab indents there were changed to tabs;
- the final blank line was removed from `native/libspeexdsp/os_support.h`.

No C tokens or behavior changed. `native/speex/speexdsp_config_types.h` is the
other local generated file. It is the concrete, portable expansion of upstream
`include/speex/speexdsp_config_types.h.in`: it includes `<stdint.h>` and maps
the 16- and 32-bit signed/unsigned Speex types to the corresponding standard
integer types.

`COPYING` is the upstream BSD 3-Clause license. The vendored source files
also retain their original copyright and license notices. This integration
contains no model or weight files. In particular, it does not include
RNNoise weights.

The Go adapters compile this source directly with CGO, so ordinary builds do
not depend on a globally installed `libspeexdsp` package. The supported
production image already builds with `CGO_ENABLED=1`; a C compiler is required
for native builds. The denoiser uses SpeexDSP preprocessing on 10 ms mono
PCM16LE frames. The resampler uses the desktop quality setting (`5`) and a
bounded streaming adapter.
