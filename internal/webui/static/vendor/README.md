# Vendored files

The UI has no build step (plan-m4 Q5). Its one dependency is vendored here, at a pinned version:

| File | What | From | sha256 | Licence |
| --- | --- | --- | --- | --- |
| `htm-preact-3.1.1.module.js` | htm 3.1.1's standalone module: Preact 10, its hooks, and htm, in one ES module | `https://unpkg.com/htm@3.1.1/preact/standalone.module.js` | `72284e8e9079c87817145df1110f74e8a2aa040b2fc384922e18dfcb46fc1fd7` | htm: Apache-2.0; Preact: MIT |

To update it, fetch the new version, check its sha256, replace the file, and change this table.
