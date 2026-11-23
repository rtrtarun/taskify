import { useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useMemo, useSyncExternalStore, type ReactNode } from "react";
import { setAuthHooks } from "../api/client";
import { session as defaultSession, type Identity, type SessionStore } from "./session";

interface AuthValue {
  identity: Identity | null;
  notice: string | null;
  isAdmin: boolean;
  signIn: (token: string) => Promise<Identity>;
  signOut: () => void;
}

const AuthContext = createContext<AuthValue | null>(null);

export function AuthProvider({ children, store = defaultSession }: { children: ReactNode; store?: SessionStore }) {
  const state = useSyncExternalStore(store.subscribe, store.getSnapshot);
  const queryClient = useQueryClient();

  // The API client takes its token from the store and reports a 401 to it.
  useEffect(() => {
    setAuthHooks({ getToken: store.getToken, onUnauthorized: store.expire });
  }, [store]);

  // Nothing one person loaded may stay in the cache for the next.
  const signedIn = state.identity !== null;
  useEffect(() => {
    if (!signedIn) queryClient.clear();
  }, [signedIn, queryClient]);

  const value = useMemo<AuthValue>(
    () => ({
      identity: state.identity,
      notice: state.notice,
      isAdmin: state.identity?.role === "admin",
      signIn: (token) => store.signIn(token),
      signOut: () => store.signOut(),
    }),
    [state, store],
  );
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthValue {
  const v = useContext(AuthContext);
  if (!v) throw new Error("useAuth outside AuthProvider");
  return v;
}
