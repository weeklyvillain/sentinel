import { createContext, ReactNode, useContext } from "react";
import { api, Me, Service, Status } from "./api";
import { useAsync } from "./components/ui";

// Data several pages need, fetched once for the whole app and polled, so
// pages do not each load and poll their own copy.

/** The signed-in user and server info (GET /api/me), provided by App. */
export const MeCtx = createContext<Me | undefined>(undefined);
export const useMe = () => useContext(MeCtx)!;

interface Shared<T> {
  data?: T;
  error?: string;
  reload: () => Promise<void>;
}

const StatusCtx = createContext<Shared<Status>>({ reload: async () => {} });
const ServicesCtx = createContext<Shared<Service[]>>({ reload: async () => {} });

/** Sidebar counts and setup state (GET /api/status). */
export const useStatus = () => useContext(StatusCtx);
/** Every known service (GET /api/services). Call reload() after changing one. */
export const useServices = () => useContext(ServicesCtx);

export function SharedData({ children }: { children: ReactNode }) {
  const status = useAsync(() => api.get<Status>("/api/status"), [], 15000);
  const services = useAsync(() => api.get<Service[]>("/api/services"), [], 15000);
  const reloadServices = async () => {
    await services.reload();
    status.reload(); // watching a service changes the setup checklist
  };
  return (
    <StatusCtx.Provider value={{ data: status.data, error: status.error, reload: status.reload }}>
      <ServicesCtx.Provider value={{ data: services.data, error: services.error, reload: reloadServices }}>{children}</ServicesCtx.Provider>
    </StatusCtx.Provider>
  );
}
