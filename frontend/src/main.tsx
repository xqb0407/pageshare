import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { ThemeProvider } from "next-themes";
import App from "./App";
import "./index.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ThemeProvider
      attribute="data-theme"
      storageKey="ps.theme"
      defaultTheme="light"
      enableSystem={false}
    >
      <App />
    </ThemeProvider>
  </StrictMode>,
);
