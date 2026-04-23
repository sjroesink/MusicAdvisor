import { useMemo, useState } from "react";
import type { AlbumType } from "../data";
import { AlbumCard } from "../components/AlbumCard";
import { SectionHeader } from "../components/SectionHeader";
import { SkeletonCard } from "../components/SkeletonCard";
import { TypeFilter } from "../components/TypeFilter";
import type { MusicAdvisor } from "../hooks/useMusicAdvisor";
import type { CardVariant, Density } from "../types";

interface ReleasesBlockProps {
  advisor: MusicAdvisor;
  density: Density;
  variant?: CardVariant;
  showSkeletonWhileLoading?: boolean;
}

export function ReleasesBlock({
  advisor,
  density,
  variant = "list",
  showSkeletonWhileLoading = false,
}: ReleasesBlockProps) {
  const allItems = advisor.newReleases;
  const [typeFilter, setTypeFilter] = useState<ReadonlySet<AlbumType>>(
    () => new Set(),
  );

  const items = useMemo(
    () =>
      typeFilter.size === 0
        ? allItems
        : allItems.filter((i) => typeFilter.has(i.type)),
    [allItems, typeFilter],
  );

  const toggle = (t: AlbumType) =>
    setTypeFilter((s) => {
      const n = new Set(s);
      if (n.has(t)) n.delete(t);
      else n.add(t);
      return n;
    });
  const clear = () => setTypeFilter(new Set());

  const showSkeletons =
    advisor.stage === "loading" &&
    allItems.length === 0 &&
    showSkeletonWhileLoading;

  return (
    <section>
      <SectionHeader
        eyebrow="New releases"
        title="Fresh from artists you follow"
        subtitle="Albums, EPs and singles released in the last 90 days by artists you've saved or listen to often."
        count={items.length || null}
        right={
          allItems.length > 1 ? (
            <TypeFilter
              items={allItems}
              active={typeFilter}
              onToggle={toggle}
              onClear={clear}
            />
          ) : null
        }
      />
      {showSkeletons && (
        <div style={{ display: "grid", gap: 18 }}>
          <SkeletonCard density={density} />
          <SkeletonCard density={density} />
          <SkeletonCard density={density} />
        </div>
      )}
      {!showSkeletons && items.length === 0 && advisor.stage === "loading" && (
        <div
          style={{
            color: "var(--ink-faint)",
            fontSize: 13.5,
            padding: "40px 0",
            textAlign: "center",
            fontStyle: "italic",
          }}
        >
          Checking for new releases…
        </div>
      )}
      {variant === "grid" ? (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(auto-fill, minmax(220px, 1fr))",
            gap: density === "compact" ? 24 : 36,
          }}
        >
          {items.map((r) => (
            <AlbumCard
              key={r.id}
              item={r}
              kind="release"
              variant="grid"
              density={density}
              onDismiss={advisor.onDismiss}
              onRate={advisor.onRate}
              dismissed={advisor.dismissed.has(r.id)}
              rating={advisor.ratings[r.id] ?? null}
              onFilterType={toggle}
              activeTypes={typeFilter}
            />
          ))}
        </div>
      ) : (
        <div>
          {items.map((r) => (
            <AlbumCard
              key={r.id}
              item={r}
              kind="release"
              variant="list"
              density={density}
              onDismiss={advisor.onDismiss}
              onRate={advisor.onRate}
              dismissed={advisor.dismissed.has(r.id)}
              rating={advisor.ratings[r.id] ?? null}
              onFilterType={toggle}
              activeTypes={typeFilter}
            />
          ))}
        </div>
      )}
    </section>
  );
}
