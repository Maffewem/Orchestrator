import { useNavigate } from "react-router";
import { api } from "../api";
import { useCrumbs } from "../components/Layout";
import { Card, PageHeader } from "../components/ui";
import ProjectForm from "./ProjectForm";

export default function NewProjectPage() {
  useCrumbs([{ label: "Projects", to: "/projects" }, { label: "Create project" }]);
  const nav = useNavigate();
  return (
    <div className="mx-auto max-w-3xl">
      <PageHeader title="Create a project" back={{ to: "/projects", label: "Back to projects" }} />
      <Card className="p-6">
        <ProjectForm
          submitLabel="Save and deploy"
          onSubmit={async (input, previews) => {
            const p = await api.createProject(input);
            if (previews?.enabled) await api.setPreviewSettings(p.id, previews).catch(() => {});
            try {
              const d = await api.deployProject(p.id);
              nav(`/deployments/${d.id}`);
            } catch {
              nav(`/projects/${p.id}`);
            }
          }}
        />
      </Card>
    </div>
  );
}
