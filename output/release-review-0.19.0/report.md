# Calibration review — v0.19.0

The release adds model-level pooling and explicit provisional mode estimates to use sparse comparisons without claiming independently measured token prices. Unsupported components retain priors; sufficient independent contrasts enable separate component calibration.

## Algorithm and review constraints

- Fit quota growth from token contributions with an account/window/cycle capacity scale, Gaussian log priors, robust residuals and time decay. Remove cycle capacity and other effects before assessing independent information.
- Use a disjoint model-level pooled factor when no token component can be independently identified. Promote it only after chronological validation has at least four target-model crossings and improves mean absolute prediction error by at least 5%. This is an engineering gate, not a statistical significance claim.
- Retain explicitly labeled, prior-regularized provisional Fast/long-context estimates when genuine weak contrasts exist. If the two modes are confounded and feature-level co-exposure confirms the combination, estimate only Fast + long context. Do not infer a combined request rate from correlated aggregate totals alone.
- Pooled allocations affect only observed token components. Per-request values remain additive with segment totals. Only the input anchor fixes a price unit; all components, including priors and cache writes, use the same conversion.
- Distinguish published purchased-credit Fast pricing (2×) from included-quota Fast priors (normally 2.5×). New crossings update capacity with bounded precision and process uncertainty so old evidence cannot freeze adaptation.

## Evaluation protocol

The snapshot contains 722 eligible primary-quota crossings at the previously deployed lag of 2. Model factors are fitted before the prediction interval and held fixed during that interval. Capacity updates occur only after each prediction. Original usage and account identifiers are not included in this report.

The first candidate overused hard rejection of uncertain mode parameters. It worsened the recent 10/25-observation diagnostics by 15–21% and was rejected before release. Training-only inspection showed that it discarded information about Fast + long context when neither standalone multiplier could be identified. The revision was developed from that structural diagnosis, synthetic cases and earlier chronological development splits. The later 145 crossings were already inspected during review and are therefore reported as reused diagnostics, not an untouched final test.

### Fixed earlier development splits

Origins are 200, 300, 400 and 500; each uses the preceding N observations and predicts the next 20. Every evaluated target is before crossing 521, within the original first 577 training observations. Results below average the four origins. Lower MAE is better; units are percentage points of quota per crossing.

| Training observations | v0.18.1 MAE | v0.19.0 MAE | Relative change |
| --- | --- | --- | --- |
| 10 | 0.092986 | 0.092718 | -0.29% |
| 25 | 0.086991 | 0.085657 | -1.53% |
| 50 | 0.086303 | 0.084661 | -1.90% |
| 100 | 0.082166 | 0.080353 | -2.21% |

### Reused recent diagnostics

| Training observations | Evaluation observations | v0.18.1 MAE | v0.19.0 MAE | Relative change |
| --- | --- | --- | --- | --- |
| 577 | 145 | 0.091917 | 0.092797 | +0.96% |
| 10 | 20 | 0.107690 | 0.104204 | -3.24% |
| 25 | 20 | 0.100975 | 0.102049 | +1.06% |
| 50 | 20 | 0.111236 | 0.112295 | +0.95% |
| 100 | 20 | 0.110653 | 0.112125 | +1.33% |

The recent 10-observation case improves by about 3.2%; the other recent cases and the overall 145-observation diagnostic remain close to the old release. These results do not establish universal accuracy gains or independent identification of rates in fixed-composition traffic. The fixed earlier development means improve modestly at every tested training size.

## Reproduction

The optional review test copies its source database before migration:

```sh
REVIEW_SNAPSHOT=/path/to/snapshot.sqlite REVIEW_OUTPUT=/tmp/review.json \
  go test -run TestReviewerFrozenTail -count=1 .

REVIEW_SNAPSHOT=/path/to/snapshot.sqlite REVIEW_DEVELOPMENT_OUTPUT=/tmp/development.json \
  go test -run TestReviewerEarlyDevelopment -count=1 .
```

Snapshot SHA256: `02a37b4cc5402fe570419b10a50387de4153b3deaf4c3b6048dd0d2461f12671`. The source remains private; a different snapshot will produce different results.

## Validation status

- Full Go test suite, including independent review regressions: passed.
- Race detector: passed.
- Browser integration: passed in both languages and mobile layouts, including prior intervals, provisional/pooled sources and combined-mode labeling.
- Native Linux plugin build: passed.
- Markdown, JavaScript syntax and Git diff checks: passed.
- Full snapshot replay, including both rolling-backtest passes and 141,005 historical attributions: passed in 58.23 seconds locally.
- Isolated CPA v8 startup/migration with networking disabled, 0.75 CPU and a private database copy: passed in 335.65 seconds; fit schema 3, 878 selected segments and 10 models; management routes verified and test container removed.
- Cross-platform assets use the standard plugin zip layout and a release-level `checksums.txt`; download verification is part of the release/deployment step.

Official pricing references: [Codex pricing](https://learn.chatgpt.com/docs/pricing), [GPT-6.1 Sol API model](https://developers.openai.com/api/docs/models/gpt-6.1-sol).
