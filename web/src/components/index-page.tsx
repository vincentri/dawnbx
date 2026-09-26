import { useNavigate } from "@tanstack/react-router";
import { useEffect } from "react";
import { Sandboxes } from "@/pages/sandboxes";
import { useServer } from "@/pages/shell";

/**
 * The landing page is the sandboxes list, because that is where an agent starts.
 *
 * On a control plane `/v1/sandboxes` is a hard 503, so rendering it there put an
 * error above a live create form — the target deployment mode opened on an error
 * page. The nav already hid the link, which is exactly why it went unnoticed: the
 * link was never the problem, the landing route was.
 *
 * It redirects rather than rendering the cluster list in place, because that
 * page resolves its own search params from the `/clusters` route and cannot
 * mount under a different one.
 */
export function IndexPage() {
  const { settled, controlPlane } = useServer();
  const navigate = useNavigate();

  useEffect(() => {
    if (settled && controlPlane) {
      navigate({ to: "/clusters" });
    }
  }, [settled, controlPlane, navigate]);

  // Nothing until the probe answers, in either mode. A cluster pays one round
  // trip at load; a control plane does not pay for a 503 it cannot use.
  if (!settled) {
    return null;
  }
  if (controlPlane) {
    return null;
  }
  return <Sandboxes />;
}
