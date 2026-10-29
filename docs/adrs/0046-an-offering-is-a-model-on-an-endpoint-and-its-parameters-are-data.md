# 0046. An offering is a model on an endpoint, and its parameters are data

- **Status:** Accepted
- **Date:** 2026-10-10
- **Source:** PR #107 (commit 57cc6d8), recorded as D-46 in `DESIGN_DECISIONS.md`

## Decision

A model's facts (context window, output limit, the modalities its weights take and produce) hold wherever it is served. What may be sent does not: the same model accepts different parameters, media, locators, service tiers and prices on its vendor's API, on a cloud platform and behind a compatible proxy. The catalog therefore keeps three things apart. A model holds the facts, an endpoint says how a surface is reached (auth by secret reference, location, transport, batch mode, data handling, file store), and an offering joins one model to one endpoint with everything that holds there.
An offering's parameter specification is data, not adapter code: each parameter's type, range or values, default and the constraints between parameters (such as sampling allowed only with reasoning effort `none`). Dials compile against it, the router checks eligibility against it and a preset entry is validated against it at load (D-37), so a request the endpoint would reject fails before it is sent, and a new model needs a catalog row, not a release.
Modality limits, accepted source forms and per-modality prices live on the offering too, because they differ by endpoint: the conversion planner reads them (D-45), and a usage the rate card does not price leaves the call unpriced rather than billed as text (D-09).
Batch is a service tier of the offering, not a separate model, and its transport needs the endpoint's batch mode. Model, profile, preset and vendor names are distinct types, and a target names exactly one of a model, a profile or a preset, so a string meant for one is never read as another.
Version 1 catalog files still load: each row upgrades to a model and an offering on the vendor's primary endpoint, with a warning, and `saige catalog migrate` writes the version 2 form.
