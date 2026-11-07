# NewNexus visual system

- Product: self-hosted, Chinese-first network operations console.
- Primary material: real configuration, probe results, DNS diffs, jobs and audit events.
- Identity: Network icon with NewNexus wordmark; no imitation Cloudflare logo.
- Foundation: shadcn/ui Base UI components, source owned in `src/components/ui`.
- Color: neutral grayscale surfaces; blue actions, selection and focus; green/amber/red semantic state with text.
- Typography: native CJK system stack, 14px body, 12px table cells, 22px page headings, monospace hostnames/IPs.
- Spacing: 4 / 8 / 12 / 16 / 20 / 24 / 32 px; radius 4 / 6 / 8 px.
- Elevation: borders for persistent surfaces, shadows only on overlays.
- Theme: light, dark and system; system default; stored locally.
- Motion: original ease-out/ease-in-out/drawer tokens, transform/opacity only; instant navigation, filtering, sorting and real-time metrics.
- Every overlay retains focus restoration, keyboard handling and reduced-motion behavior.
