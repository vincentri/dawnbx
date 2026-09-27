// The one place a failed request is shown. It lives here rather than on a page
// because a page that renders its own tables can forget: Settings did, and a
// failed GET painted its empty state, so "no API keys" was the dashboard's
// answer to a database that could not be read.
//
// The rule it exists to keep: a query error is shown where the query was made,
// not only toasted. main.tsx's toast cache covers mutations, so a read that
// fails is silent unless something like this renders it.
export function Fail({ e }: { e: unknown }) {
  return <p className="text-sm text-destructive">{textOf(e)}</p>;
}

/**
 * The message of a thrown thing, and the narrowing that makes it printable.
 * Shared because every page that shows a failure needs the same answer, and
 * two copies of it is how a page ends up rendering `[object Object]`.
 */
export const textOf = (e: unknown) => (e instanceof Error ? e.message : String(e));
