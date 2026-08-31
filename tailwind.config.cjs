/** Tailwind build config for the embedded console (make webcss). */
module.exports = {
  content: ["./web/index.html", "./web/app.js", "./web/login.js"],
  theme: {
    extend: {
      colors: {
        ink: "#0a0f16",
        panel: "#111826",
        edge: "#1e2939",
        accent: "#4cc2ff",
        good: "#3fd08a",
        warn: "#f0b429",
        bad: "#f16a6a",
      },
      fontFamily: {
        mono: ["ui-monospace", "SFMono-Regular", "Menlo", "Consolas", "monospace"],
      },
    },
  },
  plugins: [],
};
