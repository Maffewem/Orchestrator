import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { DialogHost } from "./components/dialogs";
import "./index.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
    <DialogHost />
  </StrictMode>,
);
