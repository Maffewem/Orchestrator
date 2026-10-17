import { createContext, useContext } from "react";
import type { Me, Role } from "../api";

const MeContext = createContext<Me | null>(null);
export const MeProvider = MeContext.Provider;

export function useMe(): Me | null {
  return useContext(MeContext);
}

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 };

/** True when the signed-in user has at least this role. */
export function useCan(role: Role): boolean {
  const me = useMe();
  return !!me && rank[me.role] >= rank[role];
}
