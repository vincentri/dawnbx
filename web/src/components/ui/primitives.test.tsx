// @vitest-environment jsdom
//
// A smoke tier by design. These are shadcn primitives, not our code: nothing
// here asserts Radix's own behaviour, and nothing here should be read as a
// spec for it. What it does assert is the contract our pages depend on — a
// primitive still renders its children, still forwards the className we hand
// it, and still exposes the role or label association a page queries for. A
// broken import, a shadcn upgrade that renames a slot or drops a role, or an
// edit to a variant's className fails here instead of surfacing as a mystery
// in one of the page suites. Keep it one file and keep it cheap.
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement } from "react";
import { useState } from "react";
import { toast } from "sonner";
import { describe, expect, it, vi } from "vitest";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Toaster } from "@/components/ui/sonner";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

/** The className every part is handed; it must survive onto the DOM node. */
const PROBE = "probe-class";

/** Structural parts carry no text of their own; the slot is how we find them. */
const bySlot = (slot: string) => document.querySelector<HTMLElement>(`[data-slot="${slot}"]`)!;

type Part = {
  /** how a caller reaches the element — the query the pages use. */
  find: () => HTMLElement;
  /** text the part must still show, proving children survive */
  text?: string;
  /** attributes a caller can rely on */
  attrs?: Record<string, string>;
  /**
   * The className this part must carry: the probe we handed it by default, or
   * `false` for an element the primitive renders itself, which never sees one.
   */
  className?: string | false;
};

type Slot = { name: string; node: ReactElement; parts: Part[] };

const slots: Slot[] = [
  {
    name: "Button",
    node: <Button className={PROBE}>Create</Button>,
    parts: [{ find: () => screen.getByRole("button", { name: "Create" }), text: "Create" }],
  },
  {
    name: "Badge",
    node: <Badge className={PROBE}>running</Badge>,
    parts: [{ find: () => screen.getByText("running"), text: "running" }],
  },
  {
    name: "Card",
    node: (
      <Card className={PROBE}>
        <CardHeader className={PROBE}>
          <CardTitle className={PROBE}>Quota</CardTitle>
          <CardDescription className={PROBE}>Four sandboxes</CardDescription>
          <CardAction className={PROBE}>Edit</CardAction>
        </CardHeader>
        <CardContent className={PROBE}>Body</CardContent>
        <CardFooter className={PROBE}>Footer</CardFooter>
      </Card>
    ),
    parts: [
      { find: () => bySlot("card") },
      { find: () => bySlot("card-header") },
      { find: () => screen.getByText("Quota"), text: "Quota" },
      { find: () => screen.getByText("Four sandboxes"), text: "Four sandboxes" },
      { find: () => screen.getByText("Edit"), text: "Edit" },
      { find: () => screen.getByText("Body"), text: "Body" },
      { find: () => screen.getByText("Footer"), text: "Footer" },
    ],
  },
  {
    name: "Table",
    node: (
      <Table className={PROBE}>
        <TableCaption className={PROBE}>Live sandboxes</TableCaption>
        <TableHeader className={PROBE}>
          <TableRow className={PROBE}>
            <TableHead className={PROBE}>Name</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody className={PROBE}>
          <TableRow className={PROBE}>
            <TableCell className={PROBE}>box-1</TableCell>
          </TableRow>
        </TableBody>
        <TableFooter className={PROBE}>
          <TableRow className={PROBE}>
            <TableCell className={PROBE}>1 sandbox</TableCell>
          </TableRow>
        </TableFooter>
      </Table>
    ),
    parts: [
      { find: () => screen.getByRole("table") },
      { find: () => bySlot("table-header") },
      { find: () => bySlot("table-body") },
      { find: () => bySlot("table-footer") },
      { find: () => screen.getByText("Live sandboxes"), text: "Live sandboxes" },
      { find: () => screen.getByRole("columnheader", { name: "Name" }), text: "Name" },
      { find: () => screen.getByRole("cell", { name: "box-1" }), text: "box-1" },
      { find: () => screen.getByRole("cell", { name: "1 sandbox" }), text: "1 sandbox" },
    ],
  },
  {
    name: "Label",
    node: <Label className={PROBE}>API key</Label>,
    parts: [{ find: () => screen.getByText("API key"), text: "API key" }],
  },
  {
    name: "Input",
    // No children to preserve: the contract is the box, its label and its type.
    node: <Input className={PROBE} type="password" aria-label="Token" defaultValue="hunter2" />,
    parts: [
      {
        find: () => screen.getByLabelText("Token"),
        attrs: { type: "password", "data-slot": "input" },
      },
    ],
  },
  {
    name: "Dialog",
    node: (
      <Dialog defaultOpen>
        <DialogContent className={PROBE} showCloseButton={false}>
          <DialogHeader className={PROBE}>
            <DialogTitle className={PROBE}>Revoke?</DialogTitle>
            <DialogDescription className={PROBE}>Clients stop working.</DialogDescription>
          </DialogHeader>
          <DialogFooter className={PROBE} showCloseButton>
            <Button className={PROBE} variant="outline">
              Cancel
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    ),
    parts: [
      { find: () => bySlot("dialog-content") },
      { find: () => bySlot("dialog-header") },
      { find: () => bySlot("dialog-footer") },
      { find: () => screen.getByRole("button", { name: "Cancel" }), text: "Cancel" },
      // Close is rendered by DialogFooter itself, so it carries no className
      // of ours; the point is that showCloseButton puts it in the footer.
      {
        find: () => screen.getByRole("button", { name: "Close" }),
        text: "Close",
        className: false,
      },
    ],
  },
  {
    name: "Tabs",
    node: (
      <Tabs className={PROBE} defaultValue="keys">
        <TabsList className={PROBE} variant="line">
          <TabsTrigger className={PROBE} value="keys">
            API keys
          </TabsTrigger>
          <TabsTrigger className={PROBE} value="audit">
            Audit
          </TabsTrigger>
        </TabsList>
        <TabsContent className={PROBE} value="keys">
          One key
        </TabsContent>
        <TabsContent className={PROBE} value="audit">
          Two entries
        </TabsContent>
      </Tabs>
    ),
    parts: [
      { find: () => bySlot("tabs") },
      { find: () => screen.getByRole("tablist") },
      { find: () => screen.getByRole("tab", { name: "API keys" }), text: "API keys" },
      { find: () => screen.getByRole("tab", { name: "Audit" }), text: "Audit" },
      { find: () => screen.getByRole("tabpanel"), text: "One key" },
    ],
  },
  {
    name: "Select",
    node: (
      <Select defaultValue="internet">
        <SelectTrigger className={PROBE} aria-label="network">
          <SelectValue />
        </SelectTrigger>
        <SelectContent className={PROBE}>
          <SelectGroup>
            <SelectLabel className={PROBE}>Egress</SelectLabel>
            <SelectItem className={PROBE} value="internet">
              internet
            </SelectItem>
            <SelectSeparator className={PROBE} />
            <SelectItem className={PROBE} value="none">
              no network
            </SelectItem>
          </SelectGroup>
        </SelectContent>
      </Select>
    ),
    parts: [
      { find: () => bySlot("select-trigger") },
      // With the list closed, SelectValue shows the current value's own text.
      { find: () => bySlot("select-value"), text: "internet", className: false },
    ],
  },
  {
    name: "DropdownMenu",
    node: (
      <DropdownMenu defaultOpen>
        <DropdownMenuTrigger className={PROBE}>ada</DropdownMenuTrigger>
        <DropdownMenuContent className={PROBE}>
          <DropdownMenuLabel className={PROBE}>ada@acme</DropdownMenuLabel>
          {/* Our Group wrapper styles nothing, so there is no className to forward. */}
          <DropdownMenuGroup>
            <DropdownMenuItem className={PROBE}>Sign out</DropdownMenuItem>
            <DropdownMenuShortcut className={PROBE}>⌘Q</DropdownMenuShortcut>
          </DropdownMenuGroup>
          <DropdownMenuSeparator className={PROBE} />
          <DropdownMenuCheckboxItem className={PROBE} checked>
            Show stopped
          </DropdownMenuCheckboxItem>
          <DropdownMenuRadioGroup className={PROBE} defaultValue="grid">
            <DropdownMenuRadioItem className={PROBE} value="grid">
              Grid
            </DropdownMenuRadioItem>
            <DropdownMenuRadioItem className={PROBE} value="table">
              Table
            </DropdownMenuRadioItem>
          </DropdownMenuRadioGroup>
          <DropdownMenuSub defaultOpen>
            <DropdownMenuSubTrigger className={PROBE}>More</DropdownMenuSubTrigger>
            <DropdownMenuSubContent className={PROBE}>
              <DropdownMenuItem className={PROBE}>Restart</DropdownMenuItem>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
        </DropdownMenuContent>
      </DropdownMenu>
    ),
    parts: [
      { find: () => bySlot("dropdown-menu-trigger"), text: "ada" },
      { find: () => bySlot("dropdown-menu-content") },
      { find: () => screen.getByText("ada@acme"), text: "ada@acme" },
      // A styling-free wrapper: the smoke tier only checks it renders.
      { find: () => bySlot("dropdown-menu-group"), className: false },
      { find: () => screen.getByRole("menuitem", { name: "Sign out" }), text: "Sign out" },
      { find: () => screen.getByText("⌘Q"), text: "⌘Q" },
      { find: () => bySlot("dropdown-menu-separator") },
      {
        find: () => screen.getByRole("menuitemcheckbox", { name: "Show stopped" }),
        text: "Show stopped",
      },
      { find: () => bySlot("dropdown-menu-radio-group") },
      { find: () => screen.getByRole("menuitemradio", { name: "Grid" }), text: "Grid" },
      { find: () => screen.getByRole("menuitemradio", { name: "Table" }), text: "Table" },
      // Radix's Sub is a context provider with no element of its own, and its
      // content only mounts once the submenu is opened — see the submenu test.
      { find: () => bySlot("dropdown-menu-sub-trigger"), text: "More" },
    ],
  },
];

describe("shadcn primitives keep the contract the pages query for", () => {
  for (const slot of slots) {
    it(`${slot.name} renders its children and forwards className`, () => {
      render(slot.node);
      slot.parts.forEach((part, i) => {
        const el = part.find();
        const where = `${slot.name} part ${i} (${part.text ?? "no text"})`;
        expect(el, `${where}: nothing rendered for this part`).toBeTruthy();
        if (part.className !== false) expect(el, where).toHaveClass(part.className ?? PROBE);
        if (part.text !== undefined) expect(el, where).toHaveTextContent(part.text);
        for (const [attr, value] of Object.entries(part.attrs ?? {})) {
          expect(el, where).toHaveAttribute(attr, value);
        }
      });
    });
  }
});

describe("primitives a caller interacts with", () => {
  it("a Button runs its onClick", async () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Kill</Button>);

    await userEvent.click(screen.getByRole("button", { name: "Kill" }));

    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("Button and Badge render their child element when asChild", () => {
    render(
      <>
        <Button asChild className={PROBE}>
          <a href="/settings">Settings</a>
        </Button>
        <Badge asChild className={PROBE}>
          <a href="/v1/sandboxes">3 running</a>
        </Badge>
      </>,
    );

    // Slot merges the variant classes onto the child instead of wrapping it:
    // a second <button> here would break every link-styled use in the app.
    const link = screen.getByRole("link", { name: "Settings" });
    expect(link.tagName).toBe("A");
    expect(link).toHaveClass(PROBE);
    const badge = screen.getByRole("link", { name: "3 running" });
    expect(badge.tagName).toBe("A");
    expect(badge).toHaveClass(PROBE);
  });

  it("a Label points at its input", async () => {
    render(
      <>
        <Label htmlFor="new-password">New password</Label>
        <Input id="new-password" type="password" />
      </>,
    );

    // getByLabelText is how the page tests reach this field; it resolves only
    // while htmlFor and id agree.
    const field = screen.getByLabelText("New password");
    await userEvent.type(field, "brandnewsecret");

    expect(field).toHaveValue("brandnewsecret");
  });

  it("a Dialog is named by its title and described by its body, and Escape closes it", async () => {
    render(
      <Dialog>
        <DialogTrigger asChild>
          <Button>Revoke</Button>
        </DialogTrigger>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke "ci"?</DialogTitle>
            <DialogDescription>Clients using it stop working within 30 seconds.</DialogDescription>
          </DialogHeader>
        </DialogContent>
      </Dialog>,
    );

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Revoke" }));

    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveAccessibleName('Revoke "ci"?');
    expect(dialog).toHaveAccessibleDescription("Clients using it stop working within 30 seconds.");

    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("a Tabs list shows one panel at a time and switches on click", async () => {
    render(
      <Tabs defaultValue="keys">
        <TabsList>
          <TabsTrigger value="keys">API keys</TabsTrigger>
          <TabsTrigger value="audit">Audit log</TabsTrigger>
        </TabsList>
        <TabsContent value="keys">ada holds one key</TabsContent>
        <TabsContent value="audit">Nothing yet</TabsContent>
      </Tabs>,
    );

    expect(screen.getByRole("tab", { name: "API keys" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tabpanel")).toHaveTextContent("ada holds one key");
    expect(screen.queryByText("Nothing yet")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("tab", { name: "Audit log" }));

    expect(screen.getByRole("tab", { name: "Audit log" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tabpanel")).toHaveTextContent("Nothing yet");
    expect(screen.queryByText("ada holds one key")).not.toBeInTheDocument();
  });

  it("a Select trigger is a named combobox that shows the value picked from the listbox", async () => {
    function Fixture() {
      const [network, setNetwork] = useState("internet");
      return (
        <Select value={network} onValueChange={setNetwork}>
          <SelectTrigger className={PROBE} aria-label="network">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="internet">internet</SelectItem>
            <SelectItem value="none">no network</SelectItem>
          </SelectContent>
        </Select>
      );
    }
    render(<Fixture />);

    const trigger = screen.getByRole("combobox", { name: "network" });
    expect(trigger).toHaveClass(PROBE);
    expect(trigger).toHaveTextContent("internet");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();

    await userEvent.click(trigger);

    const list = await screen.findByRole("listbox");
    expect(within(list).getAllByRole("option")).toHaveLength(2);
    await userEvent.click(within(list).getByRole("option", { name: "no network" }));

    await waitFor(() => expect(trigger).toHaveTextContent("no network"));
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  });

  it("a DropdownMenu opens on its trigger and reports the item chosen", async () => {
    const onSelect = vi.fn();
    render(
      <DropdownMenu>
        <DropdownMenuTrigger className={PROBE}>ada</DropdownMenuTrigger>
        <DropdownMenuContent>
          <DropdownMenuItem className={PROBE} onSelect={onSelect}>
            Sign out
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>,
    );

    const trigger = screen.getByRole("button", { name: "ada" });
    expect(trigger).toHaveClass(PROBE);
    expect(trigger).toHaveAttribute("aria-haspopup", "menu");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();

    await userEvent.click(trigger);

    const item = await screen.findByRole("menuitem", { name: "Sign out" });
    expect(item).toHaveClass(PROBE);
    await userEvent.click(item);

    expect(onSelect).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(screen.queryByRole("menu")).not.toBeInTheDocument());
  });

  it("a submenu opens from its trigger and reports the item chosen", async () => {
    const onSelect = vi.fn();
    render(
      <DropdownMenu defaultOpen>
        <DropdownMenuTrigger>ada</DropdownMenuTrigger>
        <DropdownMenuContent>
          <DropdownMenuSub>
            <DropdownMenuSubTrigger>More</DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <DropdownMenuItem onSelect={onSelect}>Restart</DropdownMenuItem>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
        </DropdownMenuContent>
      </DropdownMenu>,
    );

    expect(screen.queryByRole("menuitem", { name: "Restart" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("menuitem", { name: "More" }));

    await userEvent.click(await screen.findByRole("menuitem", { name: "Restart" }));

    expect(onSelect).toHaveBeenCalledTimes(1);
  });

  it("the Toaster shows a toast", async () => {
    render(<Toaster />);
    // Our Toaster is only a configured Sonner; a toast that never appears
    // means the wiring (className, icons) broke rather than the app.
    act(() => {
      toast("Sandbox killed");
    });

    expect(await screen.findByText("Sandbox killed")).toBeInTheDocument();
    expect(document.querySelector(".toaster")).not.toBeNull();
  });
});
