# Plex Media Server database layout

Findings from reading a repaired PMS 1.43 library pair
(the output of a DBRepair `Auto` run on 2026-09-08 against a PMS 1.43-era library).
Everything below was derived from the schema and the row data; nothing was taken from
Plex documentation. Where a numeric code's meaning is inferred from the rows, it says so.

## 1. Files

| File | Size | Pages | Schema objects | Live data |
| --- | --- | --- | --- | --- |
| `com.plexapp.plugins.library.db` | 966 MB | 235,789 x 4 KB | 86 tables, 164 indexes, 8 triggers | all library metadata |
| `com.plexapp.plugins.library.blobs.db` | 2.4 GB | 591,233 x 4 KB | identical object list | only the `blobs` table |

Both files carry the **same schema** (the only textual difference is quoting style on
`statistics_bandwidth`). The blobs file is a full copy of the schema in which every
table is empty except `blobs` (117,926 rows), `accounts` (one placeholder row
"Administrator"), `preferences` (one row) and `schema_migrations`. The main database has
447 migration rows, the blobs database 442. The last migration in both is
`500000000001.231`. Both use `journal_mode=delete`, no auto-vacuum, UTF-8, page size
4096, no freelist pages (they were just rebuilt).

There are **no declared foreign keys** except on `activities`, `external_metadata_items`,
`metadata_item_clusterings` and `metadata_item_setting_markers`. Every other
relationship is by naming convention (`<table>_id`) and was verified by joining.

Column type names are custom: `dt_integer(8)` is a Unix epoch seconds integer;
`integer(8)` is a 64-bit integer; `boolean` stores `'t'`/`'f'` or 0/1.
Many tables carry an `extra_data` column holding a small JSON object whose keys are
namespaced (`at:`, `pv:`, `ma:`, `ex:`, `pr:`, `sr:`) plus a `url` key with the same
data URL-encoded.

## 2. The core hierarchy

```
library_sections (4)
  └─ section_locations (4)        root folders of a section
  └─ directories (11,441)          folder tree under a location (self-referencing)
  └─ metadata_items (116,022)      one row per movie / show / season / episode / extra …
       └─ metadata_items            children via parent_id (season → show, episode → season)
       └─ media_items (184,817)     one row per physical version of an item
            └─ media_parts (184,843)   one row per file of a version
                 └─ media_streams (607,409)  video / audio / subtitle streams in a file
       └─ taggings (2,219,311) ── tags (427,745)   genres, cast, chapters, posters …
       └─ metadata_relations (43,429)             item ↔ extra (trailer, featurette …)
```

### library_sections

| id | name | section_type | agent | scanner | root path |
| --- | --- | --- | --- | --- | --- |
| 2 | TV Shows | 2 | tv.plex.agents.series | Plex TV Series | /pool/Plex_Library/TV Shows |
| 3 | Kids TV Shows | 2 | tv.plex.agents.series | Plex TV Series | /pool/Plex_Library/Kids |
| 5 | Movies | 1 | tv.plex.agents.movie | Plex Movie | /pool/Movies |
| 7 | HD Movies | 1 | tv.plex.agents.movie | Plex Movie | /pool/HD_Movies |

`section_type` 1 = movie library, 2 = TV library (inferred from contents).
`section_locations.library_section_id` and `directories.library_section_id` point back
here. `directories` is a tree: each section has one root row with an empty `path`, and
`parent_directory_id` links subfolders to it. `directories.path` is relative to the root.

### metadata_items

The central table. `metadata_type` codes present, with the parent linkage observed:

| metadata_type | rows | meaning (inferred) | parent_id points to |
| --- | --- | --- | --- |
| 1 | 6,881 | movie | none |
| 2 | 851 | TV show | none |
| 3 | 3,644 | season | type 2 |
| 4 | 58,904 | episode | type 3 |
| 10 | 6 | internal placeholder rows titled `tv.plex.agents`, section id -2, `library://` guid | none |
| 12 | 43,433 | extra (trailer, featurette, behind-the-scenes); `extra_data` has `ex:extraType` | none, section id NULL |
| 15 | 337 | playlist; `extra_data` has `pv:owner` (account id) and `pv:sectionIDs` | none, section id NULL |
| 18 | 1,963 | collection; `extra_data` has `at:childCount`, `at:minYear`, `at:maxYear` | none |
| 42 | 3 | "Background Processing List", an internal playlist | none |

Every `parent_id` resolves (0 orphans). The hierarchy is exactly three levels for TV:
show → season → episode. Movies, extras, playlists and collections have no parent.

`guid` identifies the item across servers and is the join key for the per-user tables
(see section 4). Schemes seen: `plex://` (agent-matched movies/shows/seasons/episodes),
`iva://` (all 43,429 extras), `local://` (unmatched items), `collection://`,
`com.plexapp.agents.none://` (playlists), `library://` (the type-10 rows), `file://`.

Other notable columns: `title_sort` (COLLATE NOCASE, plus a second index with
`COLLATE icu_root`), `index` (season / episode number), `absolute_index`,
`originally_available_at`, `added_at`, `changed_at`, `resources_changed_at`
(sync counters), `user_fields` (`lockedFields=4|5|8…`, the fields locked against agent
refresh), `hash`, `slug`, `edition_title`, `metadata_agent_provider_group_id`.
The denormalised `tags_genre`, `tags_director`, `tags_star` etc. columns duplicate the
`taggings` data as text. `deleted_at` is NULL on every row here.

`metadata_items` has 23 indexes, the most of any table.

### media_items → media_parts → media_streams

* `media_items.metadata_item_id` → `metadata_items.id`. 0 orphans. An item has 1 to 9
  media rows (average 1.7); multiple rows are multiple versions (different encodes).
  Only movies (1), episodes (4) and extras (12) have media. Columns are the container
  level summary: `container`, `video_codec`, `audio_codec`, `width`, `height`,
  `duration`, `bitrate`, `size`, `display_aspect_ratio`, `frames_per_second`,
  `audio_channels`, `media_analysis_version`, `color_trc`. `hints` holds the scanner's
  URL-encoded name/year guess. `type_id` is NULL on every row. `channel_id`,
  `begins_at`, `ends_at` are for Live TV and unused here.
* `media_items.library_section_id` and `section_location_id` → the section and root.
  For the 116,346 extras these are NULL because extras are streamed from Plex
  (`iva://`), not stored locally.
* `media_parts.media_item_id` → `media_items.id`. 0 orphans. One part per media item in
  practice (184,843 vs 184,817). `file` is the absolute path, `directory_id` →
  `directories.id` (NULL for extras), `hash`, `open_subtitle_hash`, `size`,
  `duration`. `extra_data` carries the container profile and a `pv:chapters` JSON blob.
* `media_streams.media_part_id` → `media_parts.id`, and `media_streams.media_item_id`
  → `media_items.id` (both stored; they are consistent on every row). 3 subtitle
  stream rows point at parts that no longer exist. `stream_type_id`: 1 = video
  (184,822), 2 = audio (197,316), 3 = subtitle (225,271). `url` is set for external
  subtitle files (`file:///…srt`). `extra_data` holds codec details
  (`ma:bitDepth`, `ma:frameRate`, `ma:samplingRate`, …). `default`, `forced`,
  `language`, `index`, `channels`, `bitrate`.

### tags and taggings

`tags` is a dictionary of values; `taggings` is the many-to-many link to
`metadata_items`. Both join columns resolve on every row (0 orphans).
`tags.metadata_item_id` and `tags.parent_id` are NULL on all 427,745 rows.
`tags.key` holds an external id for people (24-hex ids). `tags.tag_type` codes seen:

| tag_type | tags | taggings | meaning (inferred from values) | tagging columns used |
| --- | --- | --- | --- | --- |
| 1 | 35 | 20,591 | genre | |
| 2 | 1,374 | 11,076 | collection membership | |
| 4 | 8,002 | 57,354 | director | |
| 5 | 15,130 | 102,000 | writer | |
| 6 | 178,813 | 1,034,445 | actor / role; `taggings.text` = character, `index` = billing order | text, index |
| 7 | 12,690 | 113,509 | producer | |
| 8 | 80 | 10,420 | country | |
| 9 | 27,179 | 111,251 | chapter; one tagging per chapter with `time_offset`/`end_time_offset` ms | time_offset, end_time_offset, index |
| 10 | 4,251 | 98,203 | review; tag = critic name, `text` = review body | text, extra_data |
| 11 | 112 | 117,751 | label / smart-collection name | |
| 12 | 1 | 89,322 | marker (intro, credits); `text` = marker type, offsets in ms, `extra_data` has `pv:version` | text, time_offset, end_time_offset |
| 42 | 3 | 0 | optimized-version profiles ("Original Quality", "Optimized for Mobile"); `extra_data` has `sr:deviceProfile` | |
| 312 | 1 | 113,270 | poster; `text` = remote URL, `thumb_url` = `metadata://posters/…` local path | text, thumb_url |
| 313 | 1 | 8,747 | background art | text, thumb_url |
| 314 | 172,317 | 177,834 | external id (`imdb://tt…`, `tmdb://…`, `tvdb://…`) | |
| 316 | 6 | 106,304 | rating source image (`imdb://image.rating`); `text` = score | text |
| 317 | 1 | 593 | theme music | text |
| 318 | 7,477 | 23,005 | studio | |
| 319 | 236 | 1,126 | network | |
| 322 | 33 | 3,228 | episode-ordering source ("TheTVDB (USA)") | |
| 323 | 1 | 7,206 | clear logo | text, thumb_url |
| 324 | 1 | 5,663 | unknown, empty tag, always has `extra_data` | extra_data |
| 325 | 1 | 6,413 | square art | text, thumb_url |

The FTS tag index (section 6) only covers types 1, 2, 4 and 6 in this database; the
trigger list adds 0, 207 and 400 which do not occur here.

### metadata_relations

Links a movie or show (`metadata_item_id`) to its extras
(`related_metadata_item_id`, always type 12). `relation_type` values 1, 3, 5 and 6 occur;
they correspond to the extra kinds (trailer, featurette, etc.). All 43,429 rows point
from type 1 or 2 to type 12.

### collections and playlists

* A collection is a `metadata_items` row of type 18 in the same section. Membership is
  stored two ways: a `tags` row of type 2 with the collection name, linked by
  `taggings`; and the item count in the collection row's `extra_data`.
* A playlist is a `metadata_items` row of type 15 with no section. Its content is in
  `play_queue_generators`: `playlist_id` → the playlist row, `metadata_item_id` → the
  member, `order` gives position. 147,561 generator rows; 251 reference a playlist row
  that no longer exists and 408 reference a missing item. `metadata_item_accounts`
  (340 rows) grants accounts access to playlists (`account_id`, `metadata_item_id`,
  all type 15 or 42).
* `play_queues` (253) are live "now playing" queues, unique per
  (`client_identifier`, `account_id`, `metadata_type`). `play_queue_items` holds their
  entries (`play_queue_id`, `metadata_item_id`, `order`). 11,648 of 16,036 items
  reference 196 `play_queue_id` values with no matching `play_queues` row (found by
  left join; the ids fall inside the range the table has issued, so they are queues that
  were replaced or removed without their items being cleaned up).

## 3. Agents and providers

* `metadata_agent_providers` (8): one row per agent (`tv.plex.agents.movie`,
  `tv.plex.agents.series`, `tv.plex.agents.music`, MusicBrainz, Personal Media,
  Local Media, NFO movie, NFO series). `metadata_types` lists the type codes each handles.
* `metadata_agent_provider_groups` (6) and `metadata_agent_provider_group_items` (6)
  group providers with an `order`. `library_sections.metadata_agent_provider_group_id`
  and `metadata_items.metadata_agent_provider_group_id` reference the group (NULL on
  every section here, so the legacy `agent` column is what is in use).
* `plugins` (13) and `plugin_prefixes` (2) are the legacy plugin registry.
* `media_provider_resources` (3): a DVR harvester with an HDHomeRun tuner and the cloud
  EPG as children (`parent_id` self-reference). No Live TV data exists elsewhere.
* `external_metadata_sources` / `external_metadata_items`: empty.

## 4. Per-user state

`accounts` (42): id 0 is a blank row, id 1 is the server owner, all others are plex.tv
user ids (which is why `sqlite_sequence` for accounts is 757,737,988).

| table | rows | key to content | key to user |
| --- | --- | --- | --- |
| `metadata_item_settings` | 38,882 | `guid` = `metadata_items.guid` | `account_id` |
| `metadata_item_views` | 22,042 | `guid`, plus denormalised titles and `grandparent_guid` | `account_id`, `device_id` |
| `media_part_settings` | 2,147 | `media_part_id` | `account_id` |
| `media_stream_settings` | 130 | `media_stream_id` (unique with account) | `account_id` |
| `media_item_settings` | 0 | `media_item_id` | `account_id` |
| `metadata_item_accounts` | 340 | `metadata_item_id` (playlists) | `account_id` |
| `library_section_permissions` | 0 | `library_section_id` | `account_id` |
| `download_queues` / `download_queue_items` | 12 / 1 | `metadata_item_id`, `media_part_id` | `owner`, `client_identifier` |
| `view_settings` | 0 | | `account_id` |

The watch-state tables are keyed by **guid, not by id**, so they survive a
re-match or a library rebuild. 9,898 settings rows and 2,448 view rows have guids with no
current `metadata_items` row (items removed after being watched, mostly
`plex://episode/…`, `plex://movie/…` and `collection://` guids); see section 9. `metadata_item_settings` carries
`view_offset` (resume point), `view_count`, `last_viewed_at`, `rating`, `skip_count`
and `changed_at` for cloud sync. `metadata_item_setting_markers` (empty) hangs off it
with a real foreign key. `metadata_item_views` is the history log, one row per play.
`devices` (520) is referenced by `metadata_item_views.device_id` and by the statistics
tables. `preferences` holds `viewStateSyncLastStateSent-<account>` cursors per user.

## 5. Server bookkeeping

* `activities` (49,109): background task log with `parent_id` self-reference and a
  real ON DELETE CASCADE. Types seen: `butler`, `library.update.item.metadata`,
  `media.generate.credits`, `media.generate.voice.activity`, `media.generate.intros`.
* `statistics_bandwidth` (66,651) and `statistics_media` (17,264): keyed by
  `account_id`, `device_id`, `timespan` (0–4 bucket size) and `at`.
  `statistics_resources` is empty.
* `hub_templates` (64): home-screen hub definitions per section (`section` is a
  section id as text, `identifier` like `movie.recentlyadded`).
* `schema_migrations`: 447 rows. Versions are either date stamps (`20240718114400`) or
  `5000000000xx.yyy` feature versions. 294 rows store `rollback_sql`.
* `sqlite_sequence` shows the high-water mark for each AUTOINCREMENT table, e.g.
  `metadata_items` 532,327 and `taggings` 19,064,149, far above the live counts.
* Empty feature tables: `locations` (R-tree) with `locatables` and `location_places`
  (photo geotagging), `metadata_item_clusters` / `metadata_item_clusterings` (photo
  timeline), `versioned_metadata_items` (optimized versions), `media_subscriptions`,
  `media_grabs`, `metadata_subscription_desired_items` (DVR), `remote_id_translation`,
  `custom_channels`.

## 6. Full-text search

Four FTS4 virtual tables, each with the usual `_content`, `_segments`, `_segdir`,
`_docsize`, `_stat` shadow tables:

| virtual table | tokenizer | rows | mirrors |
| --- | --- | --- | --- |
| `fts4_metadata_titles` | default | 0 | legacy, unused |
| `fts4_metadata_titles_icu` | `collating 'root@colStrength=primary;colAlternate=shifted'` | 116,022 | `metadata_items` |
| `fts4_tag_titles` | default | 0 | legacy, unused |
| `fts4_tag_titles_icu` | same ICU collating tokenizer | 188,224 | `tags` |

The ICU tokenizer is a Plex extension, so stock `sqlite3` cannot open the virtual
tables (`unknown tokenizer: collating`). The shadow `_content` tables are plain tables
and can be read anywhere.

Linkage is by rowid: `docid` equals `metadata_items.id` / `tags.id` on every row.
Eight triggers on `metadata_items` (title, title_sort, original_title) and `tags`
(tag, tag_type) keep the ICU tables in step. The tag triggers only index
`tag_type IN (0,1,2,4,6,207,400)`.

The indexed text is **not a straight copy** of the source columns. For every row the
stored `title` column is the item title plus a trailing space, and for episodes both
`title` and `title_sort` have the show title appended (`"Tape 1, Side A 13 Reasons Why"`).
Only 9 rows match the source title exactly. This is how PMS itself populates the
index; the triggers, which copy the columns verbatim, would produce different content.
Any tool that rebuilds these tables from `metadata_items` needs to reproduce that
concatenation or searches for episodes by show name will stop working.

Two `spellfix1` tables (`spellfix_metadata_titles`, `spellfix_tag_titles`) with
`_vocab` companions exist for fuzzy matching; they are empty.

## 7. The blobs database

`blobs` (117,926 rows) is the only populated table. All rows use `linked_id`, none use
`linked_guid`. Every blob is gzip (`1F 8B 08`).

| linked_type | blob_type | rows | total bytes | content (decompressed) | links to |
| --- | --- | --- | --- | --- | --- |
| `media_part` | 5 | 59,304 | 2.32 GB | comma-separated 32-bit integers, ~125 KB each | `media_parts.id` in the main db; episodes only |
| `media_part` | 8 | 58,568 | 40 MB | string of `0`/`1` per time slice, ~138 KB each | `media_parts.id`; movies and episodes |
| `media_stream` | 3 | 54 | 0.9 MB | SRT subtitle text | `media_streams.id` (subtitle streams) |

Type 5 exists only for episode parts and matches the `media.generate.intros` activity,
so it is the audio fingerprint used for intro detection. Type 8 exists for movies and
episodes and matches `media.generate.voice.activity`, so it is the voice-activity
timeline used for dialogue boost / credits detection. Type 3 is an extracted embedded
subtitle.

Cross-database integrity after the repair: 436 type-5 and 493 type-8 blobs point at
media parts that no longer exist, and 1 subtitle blob has no stream. 923 movie parts
and 671 episode parts have no blob at all (not yet analysed). Extras never have blobs.

The two `UNIQUE` indexes on `blobs` (`linked_type, linked_id, blob_type` and
`linked_type, linked_guid, blob_type`) are the only constraints enforcing the
one-blob-per-part-per-type rule.

## 8. Quick reference: which column joins to what

| column | references |
| --- | --- |
| `*.library_section_id` | `library_sections.id` |
| `media_items.section_location_id` | `section_locations.id` |
| `media_parts.directory_id`, `directories.parent_directory_id` | `directories.id` |
| `metadata_items.parent_id`, `media_items.metadata_item_id`, `taggings.metadata_item_id`, `metadata_relations.*_metadata_item_id`, `play_queue_generators.playlist_id` and `.metadata_item_id`, `play_queue_items.metadata_item_id`, `metadata_item_accounts.metadata_item_id`, `download_queue_items.metadata_item_id` | `metadata_items.id` |
| `media_parts.media_item_id`, `media_streams.media_item_id`, `media_item_settings.media_item_id` | `media_items.id` |
| `media_streams.media_part_id`, `media_part_settings.media_part_id`, `blobs.linked_id` (media_part) | `media_parts.id` |
| `media_stream_settings.media_stream_id`, `media_part_settings.selected_*_stream_id`, `blobs.linked_id` (media_stream) | `media_streams.id` |
| `taggings.tag_id` | `tags.id` |
| `metadata_item_settings.guid`, `metadata_item_views.guid` | `metadata_items.guid` |
| `*.account_id`, `download_queues.owner`, playlist `extra_data.pv:owner` | `accounts.id` |
| `metadata_item_views.device_id`, `statistics_*.device_id` | `devices.id` |
| `play_queue_items.play_queue_id`, `play_queues.play_queue_generator_id` | `play_queues.id`, `play_queue_generators.id` |
| `activities.parent_id`, `media_provider_resources.parent_id` | self |
| `metadata_agent_provider_group_items.*_id` | `metadata_agent_provider_groups.id`, `metadata_agent_providers.id` |
| `fts4_*_icu` docid | `metadata_items.id` / `tags.id` |

## 9. Integrity review

`PRAGMA integrity_check` passes on both files and the four declared foreign keys are
clean, so everything below is application-level: dangling ids and inconsistent values
that SQLite has no way to detect. The checks were left joins across every `*_id`
column plus a set of semantic comparisons. Findings are grouped by what they mean.

### Expected by design (not problems)

| finding | count | why it is normal |
| --- | --- | --- |
| Same `guid` on several `metadata_items` rows | 1,544 guids | 1,246 are the same title in two libraries (968 Movies + HD Movies, 222 TV + Kids). Collections and playlists also repeat per section. |
| Same `guid` twice inside one section | 298 guids | 264 are episodes: two-part or split episodes matched to one agent id, and a few re-adds pending cleanup. |
| One file shared by several `media_parts` | 118 files | All are multi-episode files in the Kids library (`S01E01-E03`), one part row per episode. |
| Same `tag` text in several `tags` rows | 6,334 | Different people with the same name: each row carries a different `key`. 5,243 are actors. |
| `tags` rows with no taggings | 761 | 758 collection names left after the collection emptied, plus the 3 optimized-version profiles. |
| `metadata_item_settings` with `last_viewed_at` but `view_count` 0 | 2,084 | In-progress playback: 1,803 have a `view_offset`. |
| Episode `metadata_items.duration` differs from media duration by over a minute | 26,551 | Agent-supplied runtime versus actual file length. |
| `media_item_count` differs from the real media count | 8 | Stale counter on items that lost a version; refreshed on next scan. |

### Leftovers from deletions (dangling references)

Plex deletes parent rows without always cascading. None of these affect playback, but
they are the rows a cleanup pass would target.

| table.column | dangling rows | points at |
| --- | --- | --- |
| `play_queue_items.play_queue_id` | 11,648 of 16,036 | 196 queues that were replaced or removed |
| `play_queue_items.play_queue_generator_id` | 11,654 | same rows as above plus 6 |
| `play_queue_items.metadata_item_id` | 1,663 | deleted items; 1,479 of them are in the dead queues anyway |
| `play_queue_generators.metadata_item_id` | 408 | items removed from playlists; 342 are in the four Arrowverse and Star Wars chronological playlists |
| `play_queue_generators.playlist_id` | 251 | playlists that were deleted |
| `play_queues.playlist_id` | 1 | a deleted playlist |
| `play_queues.total_items_count` wrong | 26 queues | counter not updated after items were removed |
| `metadata_item_settings.guid` | 9,898 of 38,882 | items no longer in the library; 7,396 carry real watch state worth keeping for re-adds |
| `metadata_item_views.guid` | 2,448 of 22,042 | history for removed items (549 movies, 1,711 episodes, 188 extras) |
| `metadata_item_views.library_section_id` | 249 | sections 1, 4 and 9, which no longer exist |
| `media_part_settings.media_part_id` | 117 | parts that were replaced; none carry a stream selection |
| `media_part_settings.selected_subtitle_stream_id` | 11 | subtitle streams removed by re-analysis |
| `media_streams.media_part_id` | 3 | external `.srt` sidecars for two South Park episodes whose part was replaced |
| `metadata_relations.metadata_item_id` | 48 | movies or shows deleted while their extras stayed |
| `metadata_items` type 12 with no relation | 4 | local extras (`file://` under `/movies`) whose parent is gone |
| `directories` with no files and no children | 75 | folders emptied since the last scan |
| `blobs.linked_id` (blobs db) | 929 media_part, 1 media_stream | analysis results for parts replaced in 2024 to 2026; about 12 MB |

### Data anomalies worth a look

| finding | count | detail |
| --- | --- | --- |
| Local media with no `media_streams` and duration 0 | 35 parts | 27 `.avi`, 6 `.mp4`, 2 `.mkv`, all TV episodes. Analysis never succeeded, so they will not direct-play cleanly. |
| Chapters whose end equals start | 154 | Zero-length chapters from embedded chapter tables, for example item 138397 chapter 5. |
| Markers (intro or credits) ending past the media duration | 2,128 | Compared to the longest version of the item with 5 seconds of slack. |
| `view_offset` greater than the item duration | 74 | Resume points beyond the agent-supplied runtime; the file is longer than the metadata says. |
| `added_at` in the future | 1 | X-Men Origins: Wolverine has `added_at` = 2147483647 (19 Jan 2038), the 32-bit maximum. It will sort first in Recently Added forever. |

### Checks that came back clean

Every `library_section_id`, `parent_id`, `directory_id`, `media_item_id`,
`taggings.tag_id`, `taggings.metadata_item_id`, `account_id`, `device_id`,
`metadata_item_accounts.*`, `download_queue_items.queue_id`, `activities.parent_id`,
`hub_templates.section` and `media_stream_settings.media_stream_id` resolves. No
duplicate episode or season numbers within a parent, no child in a different section
than its parent, no media in another section's root, no empty local files, no missing
hashes, no NULL titles outside seasons and episodes, and every AUTOINCREMENT sequence
is above its table's highest id. Both FTS `_content` tables cover exactly the rows they
should: every `metadata_items` row and every indexable tag, and nothing else.
