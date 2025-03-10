# @highlight-run/next

## 1.3.0

### Minor Changes

-   fix workspace:\* dependencies

### Patch Changes

-   Updated dependencies
    -   @highlight-run/node@1.3.0

## 2.0.0

### Major Changes

-   require project id for H.init
-   support for errors without associated sessions/requests

## 2.1.2

### Patch Changes

-   Adds support for Next.js `generateBuildId` config parameter, which will set Highlight `appVersion` if none is provided.

## 2.2.0

### Minor Changes

-   Adds ability to record `console` methods.

### 3.1.2

### Patch Changes

- Bumping to match `@highlight-run/node`

### 3.2.0

### Minor Changes

- Adding exports for `@highlight-run/next/client` and `@highlight-run/next/server`
    > We're hoping to remove `@highlight-run/next/highlight-init` and the default `@highlight-run/next` imports in favor of the new `/client` and `/server` varieties. For now we'll maintain the original imports as aliases.
- Adding `@highlight-run/node` and `highlight.run` to `peerDependencies`

### 4.0.0

### Major changes

- Removing exports for `@highlight-run/next` and `@highlight-run/next/highlight-init`. Import from `@highlight-run/next/server` and `@highlight-run/next/client` instead.
- Adding `excludedHostnames?: string[]` to `HighlightInit` props. Pass in a list of full or partial hostnames to prevent tracking: `excludedHostnames={['localhost', 'staging']}`.

### 4.1.0

### Minor changes

- Added support for setting metadata on `consumeError`

### 4.2.0

### Minor changes

- Added support for setting `serviceName`

## 4.3.1

### Minor Changes

-   Ensure console serialization works with `BigInteger` and other unserializeable types.

## 4.3.2

### Patch Changes

- Tune settings of opentelemetry SDK to reduce memory usage.
- Enable GZIP compression of exported data.

### 4.4.0

### Minor changes

- Repaired `Highlight` export from `@highlight-run/next/server` to keep serverless functions alive while flushing OTEL errors
- Added `H` and `Highlight` exports from `@highlight-run/next/edge` to wrap Vercel's Edge Runtime API endpoints
- Added `H` and `Highlight` exports from `@highlight-run/next/app-router` to wrap App Router API endpoints

### 4.4.1

### Patch changes

- Excised `@protobufjs/inquire` from the build to eliminate console warnings and repair the Edge runtime wrapper
- Exported `getHighlightErrorInitialProps` to streamline configuring `pages/_error.tsx`

### 4.4.2

### Patch changes

- Export types.
- Downgrade `@opentelemetry/api` to avoid peer dependency issue. Also, it turns out that v1.4.1 is identical to v1.6.0 due to a revert.
- Move `@opentelemetry/api` and `@opentelemetry/resources` to `devDependencies`
- Repair `use-client` declaration
- Bundle `@highlight-run/sourcemap-uploader`

### 5.0.0

### Major changes

-Moved `withHighlightConfig` to `@highlight-run/next/config` to repair bundle issues