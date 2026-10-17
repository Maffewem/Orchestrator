import { useEffect, useState } from "react";
import { BrowserRouter, Route, Routes } from "react-router";
import { api, getToken, setToken, setUnauthorizedHandler, type Me } from "./api";
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

  useEffect(() => {
    setUnauthorizedHandler(() => {
      setToken(null);
      setTok(null);
    });
  }, []);

  useEffect(() => {
    if (!token) return setMe(null);
    api.me().then(setMe, () => setMe(null));
  }, [token]);

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
