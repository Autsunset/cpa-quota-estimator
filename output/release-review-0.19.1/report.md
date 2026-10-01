# Calibration review — v0.19.1

This patch removes an unintended cold-model delay from v0.19.0. Pooled validation now chooses its chronological cutoff from the target model’s observations. Earlier background traffic still helps fit cycle capacity and other model effects, but unrelated old traffic cannot move every target observation into the validation side.

The cutoff keeps all equal-time observations together on the validation side. The existing minimum of four target-model validation crossings and 5% prediction-error improvement is unchanged.

A regression test uses the same 24 recent comparison segments twice: once alone, and once after 180 older reference-only segments. Both must be able to validate the model; adding irrelevant old history must not suppress it.

The [v0.19.0 review](../release-review-0.19.0/report.md) documents the underlying pooling, provisional/combined modifiers, price-unit conversion, validation methodology and measured comparisons. This patch does not claim that every small sample improves or that fixed-composition data independently determines three token prices.

Patch checks passed: full Go tests, race detection, the cold-model regression, timestamp-tie isolation, native build and both fixed evaluation programs. The earlier-development and reused-recent results are numerically unchanged from the final v0.19.0 results documented above. The patch changes admission for newly observed models; it does not tune a prediction threshold against those diagnostics.

Fit eligibility advances to schema 4 and the pricing migration marker changes so upgrading from either v0.18.1 or v0.19.0 refits retained observations rather than reusing a stale admission decision.

Cross-platform artifacts and runtime deployment are checked by the release process before the server update.
