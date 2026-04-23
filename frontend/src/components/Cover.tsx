import type { CSSProperties } from "react";

interface CoverProps {
  label: string;
  style?: CSSProperties;
}

export function Cover({ label, style }: CoverProps) {
  return (
    <div className="cover" style={style}>
      <div className="cover-cap">{label}</div>
    </div>
  );
}
