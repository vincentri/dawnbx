import { useState } from "react";
import { type QueryClient, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";
import { BoxIcon, LogOutIcon, SettingsIcon, UserIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, must, type Principal } from "@/lib/api";

// useMe is the signed-in dashboard user, or null. API keys can't use the dashboard.
export function useMe() {
  return useQuery({
    queryKey: ["me"],
    queryFn: async (): Promise<Principal | null> => {
      const { data } = await api.GET("/v1/me");
      return data?.user ? data : null;
    },
    staleTime: Infinity,
  });
}

// signOut shows Login and drops the old user's data. Not qc.clear(): that
// would detach Shell's observer from ["me"] and it would never see the null.
export function signOut(qc: QueryClient) {
  qc.setQueryData(["me"], null);
  qc.removeQueries({ predicate: (q) => q.queryKey[0] !== "me" });
}

export function Shell() {
  const me = useMe();
  if (me.isPending) return null;
  if (!me.data) return <Login />;
  return (
    <div className="flex h-svh flex-col">
      <header className="flex items-center gap-4 border-b px-4 py-2">
        <Link to="/" className="flex items-center gap-2 font-semibold">
          <BoxIcon className="size-4" /> dawnbx
        </Link>
        <nav className="flex gap-1 text-sm">
          <Button variant="ghost" size="sm" asChild>
            <Link to="/" activeOptions={{ exact: true, includeSearch: false }} activeProps={{ className: "bg-muted" }}>
              Sandboxes
            </Link>
          </Button>
          <Button variant="ghost" size="sm" asChild>
            <Link to="/settings" activeProps={{ className: "bg-muted" }}>
              Settings
            </Link>
          </Button>
        </nav>
        <Status />
        <UserMenu me={me.data} />
      </header>
      <main className="min-h-0 flex-1">
        <Outlet />
      </main>
    </div>
  );
}

function Status() {
  const st = useQuery({ queryKey: ["status"], queryFn: () => must(api.GET("/v1/status")), refetchInterval: 5000 });
  if (!st.data) return <div className="flex-1" />;
  const s = st.data;
  return (
    <div className="flex-1 text-right text-xs text-muted-foreground">
      {s.version} · <span className={s.free_pct < 15 ? "text-destructive" : ""}>{Math.round(s.free_pct)}% disk free</span> · warm{" "}
      {s.warm}/{s.pool_size}
    </div>
  );
}

function UserMenu({ me }: { me: Principal }) {
  const qc = useQueryClient();
  const logout = useMutation({
    mutationFn: () => api.POST("/v1/logout"),
    onSettled: () => signOut(qc),
  });
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" size="sm">
          <UserIcon /> {me.user}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuLabel className="text-xs text-muted-foreground">
          {me.admin ? "admin (all orgs)" : `member of ${me.org}`}
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuItem asChild>
          <Link to="/settings" search={{ tab: "account" }}>
            <SettingsIcon /> Account
          </Link>
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => logout.mutate()}>
          <LogOutIcon /> Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// Login is shown by Shell whenever there is no session.
function Login() {
  const qc = useQueryClient();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const login = useMutation({
    mutationFn: () => must(api.POST("/v1/login", { body: { username: username.trim(), password }, params: { header: { "X-Dawnbx": "1" } } })),
    onSuccess: (p) => qc.setQueryData(["me"], p),
    onError: () => setPassword(""),
  });
  return (
    <div className="flex min-h-svh items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <BoxIcon className="size-4" /> dawnbx
          </CardTitle>
          <CardDescription>Sign in to your sandbox server.</CardDescription>
        </CardHeader>
        <CardContent>
          <form
            className="grid gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              login.mutate();
            }}
          >
            <div className="grid gap-2">
              <Label htmlFor="user">Username</Label>
              <Input id="user" autoComplete="username" autoFocus required value={username} onChange={(e) => setUsername(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="pass">Password</Label>
              <Input
                id="pass"
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            <Button type="submit" disabled={login.isPending}>
              Sign in
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
