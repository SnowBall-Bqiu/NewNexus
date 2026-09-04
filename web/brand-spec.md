# NewNexus interface brand specification

## Product identity

- Product name: NewNexus
- Product mark: the existing Phosphor `Network` glyph used by the application shell and authentication screen
- Brand green: `#138a58`, reserved for the product mark and healthy infrastructure state
- Primary interaction color: Kumo blue (`--color-kumo-brand`)
- Product voice: concise, operational, Chinese-first, and free of marketing language

## Design direction

- Anchor: Linear / Raycast modern builder tool, adapted to Kumo UI
- Narrative role: a daily-use network operations console, not a marketing dashboard
- Viewing distance: laptop-first, with tablet and mobile operational access
- Visual temperature: quiet, technical, precise, and slightly warm in dark mode
- Density: compact enough for repeated work while preserving 44px mobile touch targets

## System

- Dark ground: `#0b0c0f`
- Dark surfaces: `#121318`, `#181a20`, and `#202229`
- Dark hairline: `rgba(255, 255, 255, 0.075)`
- Light ground: `#f4f5f7`
- Light surfaces: `#ffffff`, `#f0f1f4`, and `#e8eaee`
- Light hairline: `rgba(20, 24, 32, 0.10)`
- Content type: 14px; metadata: 12px; section heading: 15px; page heading: 21px
- Font weight: 600 for headings, 500 for emphasized data, 400 for body copy
- Letter spacing: always `0`
- Spacing: 4 / 8 / 12 / 16 / 24 / 32
- Radius: 4px for chips, 6px for controls and rows, 8px for panels and dialogs
- Shadows: overlays only; persistent panels use hairline rings without drop shadows
- Motion: Kumo defaults plus 120-160ms transform/opacity feedback; no color tween or decorative data motion

## Usage rules

- Blue is used only for primary actions, focus, links, and selection.
- Green is used only for the NewNexus mark and health/success semantics.
- Warning, danger, and information colors remain semantic and always include text.
- Hostnames, IP addresses, versions, job IDs, and commands use the mono stack at approximately `0.9em`.
- Tables carry comparable data; resource lists carry state-rich objects; cards are reserved for overview metrics and independent summaries.
- No nested cards, gradients, glow, negative tracking, `font-bold`, or page transition animation.
