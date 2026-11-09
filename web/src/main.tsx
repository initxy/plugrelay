import { RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { createAppRouter, createQueryClient } from "./app/router";
import { Providers } from "./app/providers";
import { initialLocale } from "./i18n";
import "./styles.css";

async function start() {
  const locale = initialLocale();
  // Chinese readers wait for the CJK font rules (not the font files); everyone else gets them later, for CJK in data.
  const cjk = import("./fonts-cjk.css").catch(() => undefined);
  if (locale === "zh-CN") await cjk;
  const queryClient = createQueryClient(() => {
    const next = router.state.location.href;
    queryClient.clear();
    void router.navigate({ to: "/login", search: { next } });
  });
  const router = createAppRouter(queryClient);
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <Providers queryClient={queryClient}>
        <RouterProvider router={router} />
      </Providers>
    </StrictMode>,
  );
}

void start();
