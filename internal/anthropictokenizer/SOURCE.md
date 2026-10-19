Source: https://github.com/nianzs/sub2api/tree/1cd5141b8b978ef2f5121b9805885706991e5000/backend/internal/pkg/anthropictokenizer

Go tokenizer port is redistributed under the upstream LGPL-3.0 license (LICENSE.sub2api). Embedded vocabulary has its original Anthropic MIT notice (NOTICE.md).

Local modification: parse the compressed ! rank marker before Base64 token data. This fixes initialization failure in the pinned reference. Golden text counts were cross-checked with @anthropic-ai/tokenizer 0.0.4. These counts are estimates for current Claude models, not their proprietary tokenizer.

Long text is split at whitespace near 4 KiB, or at a UTF-8 boundary for uninterrupted runs, to bound the dependency's quadratic merge cost. Counts can differ slightly at hard boundaries. This is an intentional runtime safety difference.
