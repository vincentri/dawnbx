# docs

The dawnbx documentation site. [Fumadocs](https://fumadocs.dev) on Next.js,
exported to static files.

```sh
npm install
npm run dev        # http://localhost:3000
npm run build      # static export into out/
npm start          # serve out/
```

Content lives in `content/docs/`. `meta.json` files order the sidebar; a page
you leave out of `pages` still renders but is not listed. Sidebar and homepage
labels come from `lib/shared.ts`.

`npm run types:check` regenerates route types and typechecks.

## Deploying

`npm run build` writes plain static files to `out/`, so any static host works.
Set `SITE_URL` at build time so Open Graph and Twitter tags get absolute URLs:

```sh
SITE_URL=https://dawnbx.example npm run build
```

Once the repo has a GitHub remote, set `gitConfig` in `lib/shared.ts` to get the
GitHub link in the header and the "view source" action on each page.
