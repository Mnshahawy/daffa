/**
 * The icon set.
 *
 * Hand-rolled rather than pulled from a library, for the same reason DropdownMenu is: this is
 * forty path strings, and the dependency would ship a thousand icons in order to use forty —
 * into a binary whose entire pitch is that it is one lean static file.
 *
 * One grid (24), one stroke weight, one join, everything stroked and nothing filled. That
 * consistency is most of what makes an icon set look drawn rather than collected. Portainer's
 * are collected, which is how a `Trello` glyph ended up meaning "host" and a `Paperclip` ended
 * up meaning "attach console".
 */
export const iconPaths = {
  // ── navigation ──
  compass: ['M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20z', 'm16.2 7.8-2.1 6.4-6.3 2.1 2.1-6.4z'],
  layers: [
    'M11.2 2.2a2 2 0 0 1 1.6 0l8.6 3.9a1 1 0 0 1 0 1.8l-8.6 3.9a2 2 0 0 1-1.6 0L2.6 7.9a1 1 0 0 1 0-1.8z',
    'm22 12.6-9.2 4.2a2 2 0 0 1-1.6 0L2 12.6',
    'm22 17.6-9.2 4.2a2 2 0 0 1-1.6 0L2 17.6',
  ],
  history: ['M3 12a9 9 0 1 0 3-6.7L3 8', 'M3 3v5h5', 'M12 7.5V12l3 2'],
  box: ['m21 8-9-5-9 5v8l9 5 9-5z', 'm3.3 7 8.7 5 8.7-5', 'M12 22V12'],
  archive: [
    'M3 4.5h18v4H3z',
    'M4.5 8.5v11a1.5 1.5 0 0 0 1.5 1.5h12a1.5 1.5 0 0 0 1.5-1.5v-11',
    'M10 12.5h4',
  ],
  disc: ['M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20z', 'M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6z'],
  database: [
    'M12 2c-5 0-9 1.3-9 3s4 3 9 3 9-1.3 9-3-4-3-9-3z',
    'M3 5v14c0 1.7 4 3 9 3s9-1.3 9-3V5',
    'M3 12c0 1.7 4 3 9 3s9-1.3 9-3',
  ],
  network: [
    'M9 3h6v4H9z',
    'M2 17h6v4H2z',
    'M16 17h6v4h-6z',
    'M12 7v4',
    'M5 17v-2a1 1 0 0 1 1-1h12a1 1 0 0 1 1 1v2',
  ],
  server: [
    'M3 3.5h18v6H3z',
    'M3 14.5h18v6H3z',
    'M7 6.5h.01',
    'M7 17.5h.01',
    'M12 6.5h5',
    'M12 17.5h5',
  ],
  // A key: the bit-and-bow, not a padlock. Used for backup encryption keys — a thing you HOLD and
  // hand over, which is exactly what a padlock ("locked") would fail to say.
  key: [
    'M15.5 3a5.5 5.5 0 1 0-4.9 8L10 11.6 3 18.6V21h3v-2h2v-2h1.6l1.8-1.8A5.5 5.5 0 0 0 15.5 3z',
    'M16.5 7.5h.01',
  ],
  // A file: config-shaped contents you can read.
  file: ['M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z', 'M14 2v6h6', 'M8 13h8', 'M8 17h5'],
  scroll: [
    'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z',
    'M14 2v6h6',
    'M8.5 13h7',
    'M8.5 17h4',
  ],
  cog: [
    'M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7z',
    'M19.4 15a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-1.8-.3 1.6 1.6 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.6 1.6 0 0 0-1-1.5 1.6 1.6 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.6 1.6 0 0 0 .3-1.8 1.6 1.6 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.6 1.6 0 0 0 1.5-1 1.6 1.6 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.6 1.6 0 0 0 1.8.3H9a1.6 1.6 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.6 1.6 0 0 0 1 1.5 1.6 1.6 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0-.3 1.8V9a1.6 1.6 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.6 1.6 0 0 0-1.5 1z',
  ],
  gauge: ['M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z', 'm14.1 9.9 3.5-3.5', 'M3.5 19a10 10 0 1 1 17 0'],
  users: [
    'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2',
    'M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8z',
    'M22 21v-2a4 4 0 0 0-3-3.9',
    'M16 3.1a4 4 0 0 1 0 7.8',
  ],
  plug: ['M12 22v-5', 'M9 8V2', 'M15 8V2', 'M18 8v3a6 6 0 0 1-12 0V8z'],
  shield: ['M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z'],

  // ── chrome ──
  search: ['M10.5 17a6.5 6.5 0 1 0 0-13 6.5 6.5 0 0 0 0 13z', 'm20 20-4.9-4.9'],
  // Reveal / hide a masked value. The eye, and the eye with the world's most legible "not".
  eye: ['M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z', 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z'],
  eyeOff: [
    'M10.7 5.1A9.9 9.9 0 0 1 12 5c7 0 10 7 10 7a13.2 13.2 0 0 1-1.7 2.7',
    'M6.6 6.6A13.3 13.3 0 0 0 2 12s3 7 10 7a9.9 9.9 0 0 0 5.4-1.6',
    'M9.9 9.9a3 3 0 0 0 4.2 4.2',
    'm2 2 20 20',
  ],
  chevronDown: ['m6 9 6 6 6-6'],
  chevronRight: ['m9 6 6 6-6 6'],
  chevronsLeft: ['m11 17-5-5 5-5', 'm18 17-5-5 5-5'],
  plus: ['M12 5v14', 'M5 12h14'],
  x: ['m18 6-12 12', 'm6 6 12 12'],
  check: ['m20 6-11 11-5-5'],
  logOut: ['M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4', 'm16 17 5-5-5-5', 'M21 12H9'],
  externalLink: [
    'M15 3h6v6',
    'M10 14 21 3',
    'M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6',
  ],
  copy: [
    'M9 9h10v10a2 2 0 0 1-2 2H9a2 2 0 0 1-2-2V11a2 2 0 0 1 2-2z',
    'M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1',
  ],
  pencil: ['M12 20h9', 'M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z'],
  alert: [
    'M12 9v4',
    'M12 17h.01',
    'M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z',
  ],
  filter: ['M22 3H2l8 9.5V19l4 2v-8.5z'],
  inbox: [
    'M22 12h-6l-2 3h-4l-2-3H2',
    'M5.5 5.1 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.5-6.9A2 2 0 0 0 16.8 4H7.2a2 2 0 0 0-1.7 1.1z',
  ],

  // ── theme ──
  sun: [
    'M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10z',
    'M12 1v2',
    'M12 21v2',
    'M4.2 4.2l1.4 1.4',
    'M18.4 18.4l1.4 1.4',
    'M1 12h2',
    'M21 12h2',
    'M4.2 19.8l1.4-1.4',
    'M18.4 5.6l1.4-1.4',
  ],
  moon: ['M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z'],
  monitor: ['M3 4.5h18v11H3z', 'M8 21h8', 'M12 15.5V21'],

  // ── actions ──
  play: ['m6 4 13 8-13 8z'],
  stop: ['M6 6h12v12H6z'],
  pause: ['M8 5v14', 'M16 5v14'],
  restart: ['M3 12a9 9 0 1 1 3 6.7', 'M3 18v-5h5'],
  trash: [
    'M3 6h18',
    'M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2',
    'M19 6v14a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V6',
  ],
  terminal: ['m5 8 4 4-4 4', 'M13 16h6'],
  rocket: [
    'M5 13c-1.5 1.3-2 5-2 5s3.7-.5 5-2c.7-.9.7-2.2-.1-3a2.1 2.1 0 0 0-2.9 0z',
    'M12.5 15.5 8.5 11.5a13 13 0 0 1 2-4.5C13.4 2.8 17.3 2 21 2c0 3.7-.8 7.6-5 10.5a13 13 0 0 1-3.5 3z',
    'M14.5 9.5h.01',
  ],
  download: ['M12 3v12', 'm7 10 5 5 5-5', 'M4 20h16'],
  more: ['M12 13a1 1 0 1 0 0-2 1 1 0 0 0 0 2z', 'M19 13a1 1 0 1 0 0-2 1 1 0 0 0 0 2z', 'M5 13a1 1 0 1 0 0-2 1 1 0 0 0 0 2z'],
} as const

export type IconName = keyof typeof iconPaths
