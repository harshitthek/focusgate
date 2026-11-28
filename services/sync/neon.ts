import { defineConfig } from "@neon/config/v1";

export default defineConfig({
  auth: true,
  functions: {
    focusgatesync: { name: "FocusGate sync", source: "index.mjs" }
  }
});
