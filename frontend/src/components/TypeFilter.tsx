import { useMemo } from "react";
import type { AlbumType } from "../data";

interface FilterableItem {
  type: AlbumType;
}

interface TypeFilterProps {
  items: FilterableItem[];
  active: ReadonlySet<AlbumType>;
  onToggle: (type: AlbumType) => void;
  onClear: () => void;
}

const TYPE_ORDER: AlbumType[] = ["Album", "EP", "Single"];

export function TypeFilter({ items, active, onToggle, onClear }: TypeFilterProps) {
  const types = useMemo(() => {
    const set = new Set(items.map((i) => i.type));
    return TYPE_ORDER.filter((t) => set.has(t));
  }, [items]);

  if (types.length <= 1) return null;
  const hasActive = active.size > 0;

  return (
    <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap" }}>
      <span
        style={{
          fontFamily: "var(--mono)",
          fontSize: 10,
          letterSpacing: "0.12em",
          textTransform: "uppercase",
          color: "var(--ink-faint)",
          marginRight: 4,
        }}
      >
        Filter
      </span>
      <FilterPill active={!hasActive} onClick={onClear}>
        All
      </FilterPill>
      {types.map((t) => (
        <FilterPill key={t} active={active.has(t)} onClick={() => onToggle(t)}>
          {t}
        </FilterPill>
      ))}
    </div>
  );
}

interface FilterPillProps {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}

function FilterPill({ active, onClick, children }: FilterPillProps) {
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      style={{
        padding: "5px 11px",
        borderRadius: 999,
        fontSize: 11.5,
        border: `1px solid ${active ? "var(--ink)" : "var(--rule)"}`,
        background: active ? "var(--ink)" : "transparent",
        color: active ? "var(--paper)" : "var(--ink-soft)",
        fontFamily: "var(--sans)",
        letterSpacing: "0.02em",
        transition: "background .15s, color .15s, border-color .15s",
      }}
    >
      {children}
    </button>
  );
}
