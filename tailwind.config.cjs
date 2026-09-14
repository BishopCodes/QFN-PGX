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
        // Tailwind's slate-500/600 are 4.04:1 / 2.54:1 on ink — under the 4.5:1
        // AA floor for the dense 11px labels this console is built from.
        slate: { 500: "#8798b0", 600: "#788aa3" },
      },
      fontSize: {
        "2xs": ["0.6875rem", "1rem"], // 11px — the dense label step below text-xs
      },
      fontFamily: {
        mono: ["ui-monospace", "SFMono-Regular", "Menlo", "Consolas", "monospace"],
      },
    },
  },
  plugins: [],
};
