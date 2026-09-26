// @vitest-environment jsdom
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { Confirm } from "@/components/confirm";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableRow } from "@/components/ui/table";

const TITLE = "Kill box-1?";
const BODY = "Its files are deleted. This can't be undone.";

/** The dialog as the sandbox list mounts it: a destructive button as the trigger. */
function trigger(onConfirm: () => void) {
  return (
    <Confirm title={TITLE} body={BODY} action="Kill" onConfirm={onConfirm}>
      <Button size="xs" variant="destructive">
        Kill
      </Button>
    </Confirm>
  );
}

/** Open the dialog the way a user does. */
const openDialog = async () => {
  await userEvent.click(screen.getByRole("button", { name: "Kill" }));
  return screen.findByRole("dialog", { name: TITLE });
};

describe("Confirm", () => {
  it("opens on the trigger and shows the title, the body and both buttons", async () => {
    const onConfirm = vi.fn();
    render(trigger(onConfirm));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    const dialog = await openDialog();
    // The title is the dialog's accessible name and the body its description:
    // that pairing is what a screen reader announces on open.
    expect(dialog).toHaveAccessibleName(TITLE);
    expect(dialog).toHaveAccessibleDescription(BODY);
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Kill" })).toBeInTheDocument();
    // Opening is not consenting.
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it("closes on Cancel without running the action, and reopens on the next click", async () => {
    const onConfirm = vi.fn();
    render(trigger(onConfirm));

    const dialog = await openDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(onConfirm).not.toHaveBeenCalled();

    // A second click has to work: a dialog that only opens once is unusable
    // when the user changes their mind and then reconsiders.
    expect(await openDialog()).toBeInTheDocument();
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it("closes on Escape without running the action", async () => {
    const onConfirm = vi.fn();
    render(trigger(onConfirm));

    await openDialog();
    await userEvent.keyboard("{Escape}");

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it("runs the action exactly once on Confirm and closes", async () => {
    const onConfirm = vi.fn();
    render(trigger(onConfirm));

    const dialog = await openDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Kill" }));

    expect(onConfirm).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("closes on Confirm even when the action rejects, and still opens again", async () => {
    const boom = new Error("kill failed");
    // How a page reports a failure: the action's own promise is the only path
    // to the user, so Confirm must neither swallow the rejection nor keep the
    // dialog up waiting on it.
    const reported: unknown[] = [];
    const onConfirm = vi.fn(() => {
      const p = Promise.reject(boom);
      p.catch((e: unknown) => reported.push(e));
      return p;
    });
    render(trigger(onConfirm));

    const dialog = await openDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Kill" }));

    await waitFor(() => expect(reported).toEqual([boom]));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    expect(await openDialog()).toBeInTheDocument();
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("opens without a body, announcing nothing as a description", async () => {
    render(
      <Confirm title={TITLE} action="Kill" onConfirm={vi.fn()}>
        <Button size="xs" variant="destructive">
          Kill
        </Button>
      </Confirm>,
    );

    expect(await openDialog()).toHaveAccessibleDescription("");
  });

  it("portals out of a table row, so a cell-scoped trigger still opens the dialog", async () => {
    const onConfirm = vi.fn();
    const { container } = render(
      <Table>
        <TableBody>
          <TableRow>
            <TableCell>box-1</TableCell>
            <TableCell>{trigger(onConfirm)}</TableCell>
          </TableRow>
        </TableBody>
      </Table>,
    );

    const row = container.querySelector("tr") as HTMLElement;
    const open = within(row).getByRole("button", { name: "Kill" });
    expect(row.contains(open)).toBe(true);

    await userEvent.click(open);

    const dialog = await screen.findByRole("dialog", { name: TITLE });
    // The content is a sibling of the table under <body>, not a descendant of
    // the row: a dialog rendered in place is hoisted out of the table by the
    // HTML parser, and inside an overflow container it would be clipped.
    expect(row.contains(dialog)).toBe(false);
    expect(container.contains(dialog)).toBe(false);
    expect(dialog.closest("body")).toBe(document.body);

    // The trigger keeps its place in the row, and the action still runs.
    await userEvent.click(within(dialog).getByRole("button", { name: "Kill" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(row.contains(within(row).getByRole("button", { name: "Kill" }))).toBe(true);
  });
});
