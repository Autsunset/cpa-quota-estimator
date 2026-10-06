# Reference-anchor calibration fix - v0.19.5

## Diagnosis

The running v0.19.4 database retained 75,995 GPT-5.6 Sol usage events, and the model appeared in the latest fit's eligible observations. Switching the display anchor did not delete those events.

Two code paths incorrectly excluded all three reference-model components from calibration. Only the reference input should be fixed as the internal unit. The dashboard also interpreted the absence of calibrated components or a pooled-model row as the absence of eligible samples.

## Changes

- Keep GPT-5.6 Sol input fixed at one internal unit; allow its cache and output ratios to pass the existing independent-identifiability gates.
- Include supported reference components as nuisance parameters in pooled/modifier diagnostics.
- Do not add prior-locked reference components as new fitted nuisances for other models. This preserves existing calibration evidence when the added reference components cannot be learned.
- When another independently calibrated input is selected as the display anchor, identify the former reference input's inverse ratio as calibrated and retain the denominator's uncertainty.
- Expose eligible model observations through the additive `/prices` field `observed`. Headers, details and tooltips distinguish observed-but-unidentified models from models without eligible samples in both languages.
- Advance fit eligibility to 6 so the existing observations are refitted on upgrade; no raw samples are removed and no schema change is required.

## Validation

Synthetic independent composition changes recover reference cache/output weights of 0.16 and 7 while keeping input exactly 1. Stable compositions retain priors. Cache-only evidence cannot calibrate output. Anchor round trips retain all fixture events, quota samples and the same fit object.

The server database was exported through SQLite's online backup API while both CPA and New API remained running. Snapshot evaluation opens that backup read-only.

On the snapshot, reference cache information was approximately 4.11 against the unchanged threshold of 10. Output share variation was approximately 0.020 against 0.05, with conditional information approximately 0.38 against 10. These observations must not be presented as independently calibrated cache/output prices simply because the request count is large.

An existing frozen-tail evaluation used the same 838 eligible segments and preselected lag 2 for old and new code. The last 168 segments were held out; model weights were fitted only on the preceding 670 segments. Learned mean absolute error is unchanged at 0.064803 quota percentage points; the published-Credits comparison is unchanged at 0.082598. The existing short-prefix cases of 10, 25, 50 and 100 observations are also numerically unchanged. These reused diagnostics check regressions, not general accuracy guarantees.

Checks include the complete Go suite, race detection, browser regression coverage on desktop/mobile and both languages, JavaScript syntax, bilingual README/version consistency, Git whitespace checks, and the native shared-library build. Release artifacts and the live deployment are verified separately before declaring the server fixed.
