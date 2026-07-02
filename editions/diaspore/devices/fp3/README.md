# fp3 — Fairphone 3

`fp3` is the device-tree **codename** for the Fairphone 3 (LineageOS: `device/fairphone/FP3`,
`lineage_FP3.mk`). This dir is the **device tier** (see AGENTS.md "Repository layout") under the Diaspore
(LineageOS) edition — only the bits hardcoded to *this* device:

- `vendor/overlay/.../Trebuchet/res/xml/default_workspace_5x*.xml` — the FP3 home layout (its grids).

A second LineageOS device adds a sibling `devices/<codename>/`; nothing here belongs at the edition (OS) level.
