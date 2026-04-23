import type { Density } from "../types";

interface SkeletonCardProps {
  density?: Density;
}

export function SkeletonCard({ density = "roomy" }: SkeletonCardProps) {
  const compact = density === "compact";
  return (
    <div style={{ display: "flex", gap: 14 }}>
      <div
        className="skeleton"
        style={{
          width: compact ? 56 : 80,
          height: compact ? 56 : 80,
          borderRadius: 2,
          flexShrink: 0,
        }}
      />
      <div style={{ flex: 1, paddingTop: 6 }}>
        <div
          className="skeleton"
          style={{ width: "60%", height: 13, borderRadius: 2, marginBottom: 10 }}
        />
        <div
          className="skeleton"
          style={{ width: "40%", height: 11, borderRadius: 2, marginBottom: 14 }}
        />
        <div
          className="skeleton"
          style={{ width: "80%", height: 10, borderRadius: 2 }}
        />
      </div>
    </div>
  );
}
