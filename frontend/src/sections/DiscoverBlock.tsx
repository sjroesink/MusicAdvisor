import { useMemo, useState } from "react";
import type { AlbumType } from "../data";
import { AlbumCard } from "../components/AlbumCard";
import { SectionHeader } from "../components/SectionHeader";
import { SkeletonCard } from "../components/SkeletonCard";
import { TypeFilter } from "../components/TypeFilter";
import type { MusicAdvisor } from "../hooks/useMusicAdvisor";
import type { CardVariant, Density } from "../types";

interface DiscoverBlockProps {
  advisor: MusicAdvisor;
  density: Density;
  variant?: CardVariant;
  showSkeletonWhileLoading?: boolean;
}

export function DiscoverBlock({
  advisor,
  density,
  variant = "list",
  showSkeletonWhileLoading = false,
}: DiscoverBlockProps) {
  const allItems = advisor.discover;
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
    advisor.newReleases.length > 2 &&
    showSkeletonWhileLoading;

  return (
    <section>
      <SectionHeader
        eyebrow="Discover"
        title="Worth finding next"
        subtitle="Records you haven't saved — chosen for their closeness to your library. Players, labels and scenes all counted."
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
          {items.map((d) => (
            <AlbumCard
              key={d.id}
              item={d}
              kind="discover"
              variant="grid"
              density={density}
              onDismiss={advisor.onDismiss}
              onRate={advisor.onRate}
              dismissed={advisor.dismissed.has(d.id)}
              rating={advisor.ratings[d.id] ?? null}
              onFilterType={toggle}
              activeTypes={typeFilter}
            />
          ))}
        </div>
      ) : (
        <div>
          {items.map((d) => (
            <AlbumCard
              key={d.id}
              item={d}
              kind="discover"
              variant="list"
              density={density}
              onDismiss={advisor.onDismiss}
              onRate={advisor.onRate}
              dismissed={advisor.dismissed.has(d.id)}
              rating={advisor.ratings[d.id] ?? null}
              onFilterType={toggle}
              activeTypes={typeFilter}
            />
          ))}
        </div>
      )}
    </section>
  );
}
