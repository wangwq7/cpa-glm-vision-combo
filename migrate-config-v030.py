#!/usr/bin/env python3
import pathlib
import shutil
import sys
import time

path = pathlib.Path(sys.argv[1])
text = path.read_text()
lines = text.splitlines(keepends=True)
start = next((i for i, line in enumerate(lines) if line.rstrip() == "    glm-vision-combo:"), None)
if start is None:
    raise SystemExit("glm-vision-combo plugin block was not found")
end = len(lines)
for i in range(start + 1, len(lines)):
    stripped = lines[i].strip()
    indent = len(lines[i]) - len(lines[i].lstrip(" "))
    if stripped and not stripped.startswith("#") and indent <= 4:
        end = i
        break

block = """    glm-vision-combo:
      enabled: true
      priority: 100
      combo_model: glm-5.2-vision-combo

      primary_model: glm-5.2
      primary_context_tokens: 1048576
      primary_context_budget_tokens: 930000
      text_fallback_models: []

      vision_primary_model: gpt-5.4-mini
      vision_backup_model_1: gpt-5.6-luna
      vision_backup_model_2: claude-sonnet-4-6
      vision_backup_model_3: gemini-3.1-flash-lite
      vision_context_limit: 256000
      vision_input_token_budget: 1200
      vision_output_tokens: 4000
      vision_timeout_seconds: 20

      history_attachment_mode: onDemand
      history_attachment_compact_chars: 600
      history_attachment_restore_max_attachments: 2
      max_images_per_request: 8
      max_concurrent_extractions: 2

      auto_compression_enabled: true
      auto_compression_threshold_tokens: 720000
      auto_compression_target_tokens: 12000
      auto_compression_keep_recent_turns: 8
      auto_compression_model: ""

      cache_ttl_seconds: 259200
      cache_max_entries: 2000
      cache_path: /CLIProxyAPI/plugins/data/glm-vision-combo-cache.json
      max_image_data_bytes: 12582912
      allow_remote_image_urls: true
      strict_vision_failure: true
      on_vision_failure: error
      event_log_max_entries: 100
"""

backup = path.with_name(path.name + ".before-v030-" + time.strftime("%Y%m%d-%H%M%S"))
shutil.copy2(path, backup)
path.write_text("".join(lines[:start]) + block + "".join(lines[end:]))
print("updated:", path)
print("backup:", backup)
