import type { ReactNode } from "react";

interface SectionHeaderProps {
  eyebrow: string;
  title: string;
  subtitle?: string;
  count?: number | null;
  right?: ReactNode;
}

export function SectionHeader({
  eyebrow,
  title,
  subtitle,
  count,
  right,
}: SectionHeaderProps) {
  return (
    <div style={{ marginBottom: 20 }}>
      <div
        style={{
          display: "flex",
          alignItems: "flex-end",
          justifyContent: "space-between",
          gap: 16,
          flexWrap: "wrap",
        }}
      >
        <div>
          <div className="eyebrow" style={{ marginBottom: 10 }}>
            {eyebrow}
            {count != null && (
              <span style={{ marginLeft: 10, color: "var(--ink-faint)" }}>
                · {count}
              </span>
            )}
          </div>
          <h2 className="display" style={{ fontSize: 32, margin: 0 }}>
            {title}
          </h2>
          {subtitle && (
            <p
              style={{
                fontSize: 13.5,
                color: "var(--ink-soft)",
                margin: "8px 0 0",
                maxWidth: 560,
                lineHeight: 1.5,
              }}
            >
              {subtitle}
            </p>
          )}
        </div>
        {right}
      </div>
    </div>
  );
}
