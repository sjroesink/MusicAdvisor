import { useState, type CSSProperties } from "react";

interface CoverProps {
  label: string;
  imageUrl?: string;
  style?: CSSProperties;
}

// Cover renders the real Cover Art Archive image when we have a URL and
// the request succeeds; otherwise it falls back to the initials "label"
// so there's always something visible. Image failures are tracked in
// component state so the fallback survives a re-render.
export function Cover({ label, imageUrl, style }: CoverProps) {
  const [broken, setBroken] = useState(false);
  const showImage = Boolean(imageUrl) && !broken;

  return (
    <div className="cover" style={style}>
      {showImage ? (
        <img
          className="cover-img"
          src={imageUrl}
          alt={label}
          loading="lazy"
          onError={() => setBroken(true)}
        />
      ) : (
        <div className="cover-cap">{label}</div>
      )}
    </div>
  );
}
