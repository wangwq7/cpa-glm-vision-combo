# GLM Vision Combo CPA Plugin

Native CLIProxyAPI v7 plugin which exposes `glm-5.2-vision-combo`. Images are
first transformed to compact visual memory using an ordered vision-model chain;
the rewritten request is then sent to the configured primary text model.

The plugin deliberately does **not** use CPA's session fallback. A visual model
is called only for preprocessing and GLM remains the final model for every
request.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    glm-vision-combo:
      enabled: true
      priority: 100
      combo_model: glm-5.2-vision-combo
      primary_model: glm-5.2
      vision_primary_model: your-vision-model
      vision_backup_model_1: your-backup-vision-model
      vision_backup_model_2: ""
      vision_backup_model_3: ""
      vision_context_limit: 262144
      vision_input_token_budget: 24000
      vision_output_tokens: 4000
      vision_timeout_seconds: 45
      cache_ttl_seconds: 86400
      on_vision_failure: error
```

The four visual-model fields form the ordered chain. `vision_models` remains
available as an advanced per-model override. A candidate is skipped before
invocation if its context limit cannot accommodate the projected visual request.
Successful visual descriptions are held in a bounded in-memory cache; no raw
image is persisted.

## Build

Run `make test`, then `make package-linux-amd64`. Install the generated shared
library in CPA's persistent `plugins/linux/amd64/` directory or through a CPA
plugin store.
