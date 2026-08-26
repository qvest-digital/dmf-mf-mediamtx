# Changelog

## [1.0.0-rc.4](https://github.com/qvest-digital/dmf-mf-mediamtx/compare/mxl-v1.0.0-rc.3...mxl-v1.0.0-rc.4) (2026-08-26)


### Features

* **mxl source:** publish a video and an audio flow as one path ([#27](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/27)) ([d04aad5](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/d04aad5154e14140d84b6140ec3b1341978bdf38))
* **mxl source:** publish audio flows as Opus ([#26](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/26)) ([09e5a78](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/09e5a786c5d6b6e2f1c53007e0ab5b169f9578e7))


### Bug Fixes

* **mxl source:** derive PTS from the grain index ([#25](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/25)) ([504b934](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/504b934fa689f41e124588267ef42357d864abba))


### Miscellaneous

* merge upstream mediamtx ([#22](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/22)) ([78cdf30](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/78cdf30c3fff15260dfd436177f133b021b255fb))

## [1.0.0-rc.3](https://github.com/qvest-digital/dmf-mf-mediamtx/compare/mxl-v1.0.0-rc.2...mxl-v1.0.0-rc.3) (2026-08-26)


### Bug Fixes

* **mxl source:** encode at the flow's declared rate ([#21](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/21)) ([b2c16bd](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/b2c16bdc456fb2902ceed0fe2bcf525c88bfd8b5))

## [1.0.0-rc.2](https://github.com/qvest-digital/dmf-mf-mediamtx/compare/mxl-v1.0.0-rc.1...mxl-v1.0.0-rc.2) (2026-08-04)


### Dependencies

* move onto go-mxl 1.0.0-rc.12 ([#18](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/18)) ([ea74809](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/ea748097f94d3c69a8db2f6afb1d1beb936b750a))

## [1.0.0-rc.1](https://github.com/qvest-digital/dmf-mf-mediamtx/compare/mxl-v1.0.0-rc.0...mxl-v1.0.0-rc.1) (2026-08-03)


### Features

* **mxl source:** diag counters + validity scan + adaptive backoff on invalid grains ([cb3fbaf](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/cb3fbafef5622adb0a968fa66b0b08f7d55eec99))
* **staticsources:** add MXL (Media eXchange Layer) static source ([613c20e](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/613c20ef68c454fa416113a20bf7ade2abe1b35b))


### Bug Fixes

* **ci:** base mxl runtime on libmxl-dev (libmxl:latest 404s) ([a4449a7](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/a4449a7cba3d69e31c26cdfb2837086773ad450c))
* **ci:** build mxl image via go-mxl-builder + binary swap ([9c3a311](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/9c3a3115772d1afc4b754471de0886b00d69a9d3))
* **mxl source:** self-heal when the producer recreates the flow ([8de8c19](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/8de8c198dd401e43d032fb657079e4ae19d1ed1f))
* **mxl source:** self-heal when the producer recreates the flow ([8f32d83](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/8f32d8394518e926e97940f8f9730d056df265ed))
* **mxl source:** watchdog for the pre-start phase — path wedges silently after flow recreation (DMF-397) ([af72775](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/af72775e464ac1f5c3b19ebb06ba324a8c081187))
* **mxl source:** watchdog for the pre-start phase (DMF-397) ([252f93b](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/252f93b1adb60dd45f41cb25c0fec0b66ba297e5))
* **staticsources/mxl:** live consumer behavior — freshest grain + wall-clock PTS ([519eb44](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/519eb449dfad56cf82c6abab2304b2673dbadafb))


### Build System

* **mxl:** build against the go-mxl module directly (rc.9); drop the scratch-branch clone ([0c85dbf](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/0c85dbf2a9c3d7b00f12a664c6c592f46c56c88f))
* **mxl:** drop the now-unused git from the builder (clone is gone) ([c475c02](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/c475c02a8fb327fde6d77d32c9e26656def4582f))
* pin the runtime base by digest ([#14](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/14)) ([9e4e65c](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/9e4e65c08e6790e3fea92620d0df54b52fd94471))


### Continuous Integration

* add a release train ([#16](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/16)) ([e5e1057](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/e5e105747226c47265edd0b85423cea607c776b3))
* build + push mediamtx-mxl image to GHCR on feature branch ([1edef78](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/1edef789d69fd3f68ee597a4b8120b71dda73912))
* build mxl image on feature branch + push to the demo-app package ([3c7ef81](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/3c7ef815d308a15ddb82a467785eddd0bd294683))
* make the default branch green ([#15](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/15)) ([578eca2](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/578eca2fc737fddb5b599fe83604f983101e24bf))
* **mxl:** publish version-tagged images on mxl-v* tags; drop the unused LIBMXL_TAG ([081d127](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/081d1274ee1e43cfb6e12fb91b88acd23810580c))
* **mxl:** validate the build on PRs (build-only, no publish) ([fe84f0f](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/fe84f0f5f87749d6687f2d12f2b735e8ac5a2d4f))
* push mxl image only to qvest-digital/mediamtx-mxl (its own package) ([ecbae64](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/ecbae6487474ff7c5fab8970289a07403f78efdd))


### Miscellaneous

* publish under the renamed repository ([#13](https://github.com/qvest-digital/dmf-mf-mediamtx/issues/13)) ([20c55cd](https://github.com/qvest-digital/dmf-mf-mediamtx/commit/20c55cd6d067613f607a5c319f72c2a41342bae8))
