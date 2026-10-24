import { useEffect, useState } from "react";
import { BrowserRouter, Route, Routes } from "react-router";
import { api, ApiError, getToken, setToken, setUnauthorizedHandler, type Me } from "./api";
import { MeProvider } from "./components/auth";
import UsersPage from "./pages/Users";
import Layout from "./components/Layout";
import Login from "./pages/Login";
import OverviewPage from "./pages/Overview";
import ProjectsPage from "./pages/Projects";
import NewProjectPage from "./pages/NewProject";
import ProjectPage from "./pages/Project";
import DeploymentPage from "./pages/Deployment";
import DevicesPage from "./pages/Devices";
import DevicePage from "./pages/Device";
import ContainersPage from "./pages/Containers";
import BackupsPage from "./pages/Backups";
import UpdatesPage from "./pages/Updates";
import WorkerPage from "./pages/Worker";
import ActivityPage from "./pages/Activity";
import SettingsPage from "./pages/Settings";
import { Empty } from "./components/ui";

export default function App() {
  const [token, setTok] = useState(getToken());
  const [me, setMe] = useState<Me | null>(null);
  const [meError, setMeError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    setUnauthorizedHandler(() => {
      setToken(null);
      setTok(null);
    });
  }, []);

  useEffect(() => {
    if (!token) return setMe(null);
    // A transient failure must not leave the dashboard role-less (no Settings, no username),
    // so retry quietly and then show an error with a Retry button instead.
    let cancelled = false;
    let tries = 0;
    const load = () => {
      api.me().then(
        (m) => {
          if (cancelled) return;
          setMe(m);
          setMeError(null);
        },
        (e) => {
          if (cancelled) return;
          // The server refused this identity: drop the token and go to sign-in rather than
          // leave a dashboard open that we can't vouch for.
          if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
            setToken(null);
            setTok(null);
            return;
          }
          if (++tries < 3) setTimeout(load, 1000 * tries);
          else setMeError(e instanceof Error ? e.message : "Could not load your account");
        },
      );
    };
    load();
    // Re-verify when the tab regains focus so a revoked or expired session closes the dashboard.
    const recheck = () => {
      if (document.visibilityState === "visible") api.me().then(setMe, () => {});
    };
    document.addEventListener("visibilitychange", recheck);
    return () => {
      cancelled = true;
      document.removeEventListener("visibilitychange", recheck);
    };
  }, [token, attempt]);

  if (!token) {
    return (
      <Login
        onLogin={(t) => {
          setToken(t);
          setTok(t);
        }}
      />
    );
  }

  const signOut = () => {
    api.logout().catch(() => {});
    setToken(null);
    setTok(null);
  };

  if (!me) {
    return (
      <div className="flex min-h-screen items-center justify-center p-6">
        <div className="max-w-sm text-center">
          {meError ? (
            <>
              <p className="font-medium">Couldn't load your account</p>
              <p className="mt-1 text-sm text-neutral-500">{meError}</p>
              <div className="mt-4 flex justify-center gap-2">
                <button className="rounded-lg bg-neutral-900 px-3 py-2 text-sm text-white" onClick={() => { setMeError(null); setAttempt((n) => n + 1); }}>Retry</button>
                <button className="rounded-lg border border-neutral-300 px-3 py-2 text-sm" onClick={signOut}>Sign out</button>
              </div>
            </>
          ) : (
            <p className="text-sm text-neutral-500">Loading…</p>
          )}
        </div>
      </div>
    );
  }

  return (
    <MeProvider value={me}>
    <BrowserRouter>
      <Routes>
        <Route
          element={
            <Layout onLogout={signOut} />
          }
        >
          <Route index element={<OverviewPage />} />
          <Route path="projects" element={<ProjectsPage />} />
          <Route path="projects/new" element={<NewProjectPage />} />
          <Route path="projects/:id/*" element={<ProjectPage />} />
          <Route path="deployments/:id" element={<DeploymentPage />} />
          <Route path="devices" element={<DevicesPage />} />
          <Route path="devices/:id/*" element={<DevicePage />} />
          <Route path="containers" element={<ContainersPage />} />
          <Route path="backups" element={<BackupsPage />} />
          <Route path="workers/:deviceId/:name/*" element={<WorkerPage />} />
          <Route path="updates" element={<UpdatesPage />} />
          <Route path="activity" element={<ActivityPage />} />
          <Route path="settings" element={<SettingsPage />} />
          <Route path="users" element={<UsersPage />} />
          <Route path="*" element={<Empty title="Page not found" />} />
        </Route>
      </Routes>
    </BrowserRouter>
    </MeProvider>
  );
}
