#!/bin/bash
# Usage: metrics.sh <dir containing com.plexapp.plugins.library.db and .blobs.db>  -> prints comparable metrics
S43="${PLEX_DIR:-/usr/lib/plexmediaserver}"
export LD_LIBRARY_PATH="$S43/lib"; SQ="$S43/Plex SQLite"
cd "$1" || exit 1
for db in com.plexapp.plugins.library.db com.plexapp.plugins.library.blobs.db; do
  echo "===== $db  size=$(stat -c %s $db)"
  echo "-- header: page_size page_count freelist"; "$SQ" $db 'pragma page_size; pragma page_count; pragma freelist_count;' | tr '\n' ' '; echo
  echo "-- integrity_check(20)"; "$SQ" $db 'pragma integrity_check(20);'
  echo "-- fts integrity-check"; for t in fts4_metadata_titles fts4_tag_titles fts4_metadata_titles_icu fts4_tag_titles_icu; do printf '%s: ' $t; r="$("$SQ" $db "insert into $t($t) values('integrity-check');" 2>&1)"; echo "${r:-ok}"; done
  echo "-- schema objects"; "$SQ" $db "select type, count(*) from sqlite_master group by type;" | tr '\n' ' '; echo
  echo "-- schema md5"; "$SQ" $db "select type,name,tbl_name,sql from sqlite_master order by type,name;" | md5sum
  echo "-- triggers"; "$SQ" $db "select name from sqlite_master where type='trigger' order by name;" | tr '\n' ' '; echo
  echo "-- migrations"; "$SQ" $db "select count(*), max(version) from schema_migrations;"
  echo "-- row counts"; "$SQ" $db "select 'metadata_items',count(*) from metadata_items union all select 'media_items',count(*) from media_items union all select 'media_parts',count(*) from media_parts union all select 'media_streams',count(*) from media_streams union all select 'tags',count(*) from tags union all select 'taggings',count(*) from taggings union all select 'metadata_item_settings',count(*) from metadata_item_settings union all select 'metadata_item_views',count(*) from metadata_item_views union all select 'statistics_bandwidth',count(*) from statistics_bandwidth union all select 'statistics_media',count(*) from statistics_media union all select 'play_queue_generators',count(*) from play_queue_generators union all select 'activities',count(*) from activities union all select 'blobs',count(*) from blobs union all select 'library_sections',count(*) from library_sections union all select 'accounts',count(*) from accounts union all select 'devices',count(*) from devices;"
  echo "-- fts content rows"; "$SQ" $db "select 'meta_icu',count(*) from fts4_metadata_titles_icu_content union all select 'tag_icu',count(*) from fts4_tag_titles_icu_content union all select 'meta_legacy',count(*) from fts4_metadata_titles_content union all select 'tag_legacy',count(*) from fts4_tag_titles_content union all select 'meta_icu_segdir',count(*) from fts4_metadata_titles_icu_segdir union all select 'tag_icu_segdir',count(*) from fts4_tag_titles_icu_segdir;"
  echo "-- fts search sample"; "$SQ" $db "select count(*) from fts4_metadata_titles_icu where fts4_metadata_titles_icu match 'star'; select count(*) from fts4_tag_titles_icu where fts4_tag_titles_icu match 'drama';"
  echo "-- content checksums (order-independent)"; "$SQ" $db "select 'metadata_items', sum(length(title)+id+ifnull(metadata_type,0)) from metadata_items; select 'media_parts', sum(length(file)+size) from media_parts; select 'metadata_item_views', sum(id+ifnull(account_id,0)) from metadata_item_views; select 'metadata_item_settings', sum(id+ifnull(view_count,0)) from metadata_item_settings;"
  echo "-- stat1 rows"; "$SQ" $db "select count(*) from sqlite_stat1;"
done
