// Drives @actions/cache against the cache server for the Go end-to-end test
// (cache/e2e_test.go). One operation per process, given as JSON in argv[2]:
//
//   {"op": "save", "paths": [...], "key": "..."}
//   {"op": "restore", "paths": [...], "key": "...", "restoreKeys": [...]}
//
// LUXGHA_CACHE_NODE_DIR names a directory whose node_modules holds
// @actions/cache. ACTIONS_RESULTS_URL, ACTIONS_RUNTIME_TOKEN,
// ACTIONS_CACHE_SERVICE_V2, GITHUB_WORKSPACE and RUNNER_TEMP come from the
// test. The last stdout line is "RESULT <json>".
const dir = process.env.LUXGHA_CACHE_NODE_DIR;
const cache = await import(`${dir}/node_modules/@actions/cache/lib/cache.js`);
const req = JSON.parse(process.argv[2]);
let result;
if (req.op === 'save') {
  result = { cacheId: await cache.saveCache(req.paths, req.key) };
} else if (req.op === 'restore') {
  const hit = await cache.restoreCache(req.paths, req.key, req.restoreKeys || []);
  result = { matchedKey: hit ?? null };
} else {
  throw new Error(`unknown op ${req.op}`);
}
console.log('RESULT ' + JSON.stringify(result));
