export type AlbumType = "Album" | "EP" | "Single";

export interface NewRelease {
  id: string;
  artist: string;
  title: string;
  year: number;
  date: string;
  type: AlbumType;
  tracks: number;
  length: string;
  reason: string;
  cover: string;
  coverArtUrl?: string;
}

export interface DiscoverItem {
  id: string;
  artist: string;
  title: string;
  year: number;
  type: AlbumType;
  tracks: number;
  length: string;
  reason: string;
  cover: string;
  coverArtUrl?: string;
}

export interface MusicData {
  savedArtists: string[];
  newReleases: NewRelease[];
  discover: DiscoverItem[];
}

export const MA_DATA: MusicData = {
  savedArtists: [
    "Nils Frahm",
    "Grouper",
    "Mount Kimbie",
    "Tirzah",
    "Beach House",
    "Arthur Russell",
    "Tim Hecker",
    "Caribou",
    "Aldous Harding",
    "Floating Points",
    "Low",
    "Oneohtrix Point Never",
  ],
  newReleases: [
    {
      id: "r1",
      artist: "Nils Frahm",
      title: "Paraphrases",
      year: 2026,
      date: "Apr 18",
      type: "Album",
      tracks: 11,
      length: "48 min",
      reason: "New album from an artist you've saved.",
      cover: "NF · PARAPHRASES",
    },
    {
      id: "r2",
      artist: "Beach House",
      title: "Glass Hour",
      year: 2026,
      date: "Apr 05",
      type: "EP",
      tracks: 5,
      length: "22 min",
      reason: "Follow-up EP — you saved their last three albums.",
      cover: "BH · GLASS HOUR",
    },
    {
      id: "r3",
      artist: "Floating Points",
      title: "Continuum",
      year: 2026,
      date: "Mar 29",
      type: "Single",
      tracks: 1,
      length: "9 min",
      reason: "First new material since 2023.",
      cover: "FP · CONTINUUM",
    },
    {
      id: "r4",
      artist: "Aldous Harding",
      title: "Slow Water",
      year: 2026,
      date: "Mar 14",
      type: "Album",
      tracks: 10,
      length: "41 min",
      reason: "New album — you listened to 'Warm Chris' 38 times.",
      cover: "AH · SLOW WATER",
    },
    {
      id: "r5",
      artist: "Tim Hecker",
      title: "Shards",
      year: 2026,
      date: "Mar 02",
      type: "Album",
      tracks: 8,
      length: "52 min",
      reason: "New album from an artist you've saved.",
      cover: "TH · SHARDS",
    },
    {
      id: "r6",
      artist: "Caribou",
      title: "Honey b/w Volume",
      year: 2026,
      date: "Feb 21",
      type: "Single",
      tracks: 2,
      length: "11 min",
      reason: "Two-track single.",
      cover: "CB · HONEY",
    },
  ],
  discover: [
    {
      id: "d1",
      artist: "Rachika Nayar",
      title: "Our Hands Against the Dusk",
      year: 2021,
      type: "Album",
      tracks: 9,
      length: "36 min",
      reason: "Ambient guitar textures — if you like Grouper and Tim Hecker.",
      cover: "RN · DUSK",
    },
    {
      id: "d2",
      artist: "Sam Gendel",
      title: "Cookup",
      year: 2022,
      type: "Album",
      tracks: 10,
      length: "39 min",
      reason: "Players overlap with Sam Gendel and Floating Points sessions.",
      cover: "SG · COOKUP",
    },
    {
      id: "d3",
      artist: "Ana Roxanne",
      title: "Because of a Flower",
      year: 2020,
      type: "Album",
      tracks: 8,
      length: "32 min",
      reason: "Quiet, vocal-led ambient — adjacent to Grouper.",
      cover: "AR · FLOWER",
    },
    {
      id: "d4",
      artist: "Space Afrika",
      title: "Honest Labour",
      year: 2021,
      type: "Album",
      tracks: 19,
      length: "44 min",
      reason: "Shares label & scene with Mount Kimbie.",
      cover: "SA · HONEST",
    },
    {
      id: "d5",
      artist: "claire rousay",
      title: "sentiment",
      year: 2024,
      type: "Album",
      tracks: 10,
      length: "34 min",
      reason: "Recommended for listeners of Tirzah.",
      cover: "CR · SENTIMENT",
    },
    {
      id: "d6",
      artist: "Smerz",
      title: "Big city life",
      year: 2023,
      type: "Album",
      tracks: 12,
      length: "37 min",
      reason: "Norwegian duo — if you like Tirzah and Mica Levi.",
      cover: "SM · BIG CITY",
    },
    {
      id: "d7",
      artist: "Lea Bertucci",
      title: "A Visible Length of Light",
      year: 2023,
      type: "Album",
      tracks: 6,
      length: "42 min",
      reason: "Drone/brass overlaps with Tim Hecker listens.",
      cover: "LB · LIGHT",
    },
    {
      id: "d8",
      artist: "Duval Timothy",
      title: "Meeting with a Judas Tree",
      year: 2022,
      type: "Album",
      tracks: 13,
      length: "46 min",
      reason: "Piano-led, collaborated with Mount Kimbie.",
      cover: "DT · JUDAS TREE",
    },
  ],
};
