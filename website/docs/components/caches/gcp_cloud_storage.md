---
title: gcp_cloud_storage
type: cache
status: experimental
---

<!--
     THIS FILE IS AUTOGENERATED!

     To make changes please edit the contents of:
     lib/cache/gcp_cloud_storage.go
-->

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

:::caution EXPERIMENTAL
This component is experimental and therefore subject to change or removal outside of major version releases.
:::
Use a Google Cloud Storage bucket as a cache.

```yml
# Config fields, showing default values
label: ""
gcp_cloud_storage:
  bucket: ""
```

It is not possible to atomically upload cloud storage objects exclusively when the target does not already exist, therefore this cache is not suitable for deduplication.

## Fields

### `bucket`

The Google Cloud Storage bucket to store items in.


Type: `string`  


