import { type ReactNode, useState } from "react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";

// Confirm wraps a trigger in a "are you sure" dialog; onConfirm runs on Yes.
export function Confirm(props: {
  title: string;
  body?: ReactNode;
  action: string;
  onConfirm: () => void;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>{props.children}</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{props.title}</DialogTitle>
          {props.body && <DialogDescription>{props.body}</DialogDescription>}
        </DialogHeader>
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline">Cancel</Button>
          </DialogClose>
          <Button
            variant="destructive"
            onClick={() => {
              setOpen(false);
              props.onConfirm();
            }}
          >
            {props.action}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
