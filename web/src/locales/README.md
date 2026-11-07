NewNexus currently ships with the local `zh-CN.json` dictionary. Product copy,
including accessible labels, field metadata, notices and validation messages,
is referenced through `t` from `@/lib/i18n`.

Use stable semantic keys such as `resource.deleteDescription` and
`settings.security.configureTotp`. Keep runtime data in named values:

```tsx
t('resource.createNamed', { singular: resourceLabel })
```

Placeholders use `{name}`. Missing values remain visible, and replacement values
are inserted once as text; React handles escaping at the rendering boundary.
The existing `t(key, fallback, values)` form remains available for compatibility.
Any literal fallback must match its registered dictionary entry.

`npm run check` scans TypeScript and JSX syntax trees to reject unregistered keys,
Chinese product copy outside translations, unregistered fallbacks, and missing
named interpolation values. It excludes tests, comments, imports, type literals,
and property names; no blanket file-level exceptions are used.
