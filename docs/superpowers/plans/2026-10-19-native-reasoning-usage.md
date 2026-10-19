# Preserve observed native reasoning in output usage

Scope: Kiro only, following authorized count/protocol optimization and deployment. Do not change Sub2API, cache ratios, or multiply token counts.

Evidence: native synthetic request with no thinking option returned reasoningContentEvent (289 UTF-8 bytes), signature and a tool call. Explicit disabled is rejected by the current upstream model, which advertises only adaptive. Kiro's handler clears native reasoning from its output estimate when the public request does not enable thinking. This reproduces a real undercount, but does not establish how much of the historical CCTest output gap it explains.

Implementation: separate observed native reasoning for accounting from reasoning exposed in the public response; always count actual native reasoning once when exact upstream usage is absent. Preserve authoritative native usage and client display suppression. Do not count signatures or fabricate unavailable hidden reasoning. Cover JSON, SSE and internal Claude hosted-search rounds. Add numeric diagnostics distinguishing generated and returned output and suppressed native reasoning, without logging user content.

Validation: failing regression first, green focused checks, full Go tests/vet, Linux race, real candidate and public synthetic probes. Cache totals/count endpoint unchanged. Only switch Kiro after tests; preserve data/runtime and compare other containers.

Input investigation: old Anthropic vocabulary differs inconsistently from o200k_base and is officially inaccurate from Claude 3 onward. No automatic replacement or fixed scale-up is justified. Cache creation/read lifecycle in the screenshot is internally consistent. Exact CCTest raw input or same-content authoritative counts remain needed for input calibration.
