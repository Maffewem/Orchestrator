import { useEffect, useState } from "react";
import { BrowserRouter, Route, Routes } from "react-router";
import { getToken, setToken, setUnauthorizedHandler } from "./api";
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
import ActivityPage from "./pages/Activity";
import SettingsPage from "./pages/Settings";
import { Empty } from "./components/ui";

export default function App() {
  const [token, setTok] = useState(getToken());

  useEffect(() => {
    setUnauthorizedHandler(() => {
      setToken(null);
      setTok(null);
    });
  }, []);

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

  return (
    <BrowserRouter>
      <Routes>
        <Route
          element={
            <Layout
              onLogout={() => {
                setToken(null);
                setTok(null);
              }}
            />
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
          <Route path="activity" element={<ActivityPage />} />
          <Route path="settings" element={<SettingsPage />} />
          <Route path="*" element={<Empty title="Page not found" />} />
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
