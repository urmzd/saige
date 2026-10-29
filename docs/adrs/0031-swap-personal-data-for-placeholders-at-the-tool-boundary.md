# 0031. Swap personal data for placeholders at the tool boundary

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-31 in `DESIGN_DECISIONS.md`

## Decision

Redaction is a separate policy from gates (D-08). A gate decides whether a call runs. A `ToolRedactor` decides what each side of the tool boundary sees.
`privacy.Vault` swaps each detected value for a placeholder such as `<<EMAIL_1>>`. The same value gets the same placeholder for the whole session, so the model can refer to it across turns. Permanent `[REDACTED:LABEL]` from `privacy.Redact` is a different thing: it cannot be reversed.
The loop restores arguments inside the tool step, after the gate and any approval saw the placeholders, and tokenizes the result and any error inside the same step. The provider, the tree, telemetry, and a durable runner's journal hold only placeholders.
A result that cannot be redacted is withheld as an error, never passed through. A request the provider decorator cannot tokenize is not sent.
Detectors run as one combined regular expression with a validator per pattern (Luhn, SSN ranges, IBAN mod 97, IPv4 octets), so a long digit run is not a card unless it checks out. A rejected match does not hide its text from the other patterns: the detector then scans with each pattern alone, so a phone number next to a house number is still found. A named-entity detector plugs into the same span pipeline through `privacy.Chain`, so its findings are reversible too.
Streamed text is restored with a hold-back: a suffix that could still become a placeholder waits for the next fragment, and is flushed at the end of the block, on completion, and on error. Thinking blocks are never rewritten, because their signatures cover the text the model produced.
`privacy.Provider` serves hosts that only need to keep data away from the model vendor. Its tree then holds real values.
The vault snapshot holds the original values. The host stores it with the same protection as the data.
