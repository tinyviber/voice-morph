import { defineConfig } from "mygo-cli";

export default defineConfig({
  name: "VoiceMorph",
  identifier: "dev.tinyviber.voicemorph",
  version: "0.1.0",
  copyright: "© 2026 tinyviber",
  // `mygo dev` runs devCommand and loads devUrl; `mygo build` runs
  // buildCommand and embeds frontendDist into the app.
  devUrl: "http://localhost:5173",
  devCommand: "bun run dev:web",
  buildCommand: "bun run build:web",
  frontendDist: "dist",
  bindings: "src/mygo.ts",
  out: "build",
  macos: {
    // v1 targets macOS on Intel and Apple Silicon (darwin/universal);
    // windows/linux builds are left for later.
    minimumSystemVersion: "13.0",
    infoPlist: {
      NSMicrophoneUsageDescription:
        "VoiceMorph 需要访问麦克风来做实时变声监听。",
      CFBundleDisplayName: "VoiceMorph 变声工坊",
    },
  },
});
