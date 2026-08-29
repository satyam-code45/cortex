import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

export const metadata: Metadata = {
  title: "Cortex",
  description: "Agentic knowledge system over Jira, Notion and Gmail",
};

// Applies the saved theme before first paint — as an inline script rather than
// an effect, because an effect runs after render and light-mode users would see
// a dark flash (or vice versa). localStorage can throw (private windows), so
// the whole thing is best-effort.
const themeInit = `
try {
  const t = localStorage.getItem("theme");
  if (t === "dark" || (!t && matchMedia("(prefers-color-scheme: dark)").matches)) {
    document.documentElement.classList.add("dark");
  }
} catch {}
`;

// Explicit children prop rather than Next's generated LayoutProps: that type
// only exists after a dev/build run has written .next/types, and the check
// gate runs bare `tsc --noEmit`.
export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html
      lang="en"
      suppressHydrationWarning
      className={`${geistSans.variable} ${geistMono.variable} h-full antialiased`}
    >
      <body className="h-dvh bg-background text-foreground">
        <script dangerouslySetInnerHTML={{ __html: themeInit }} />
        {children}
      </body>
    </html>
  );
}
