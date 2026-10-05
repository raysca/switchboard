const assert = require('node:assert/strict');
const { createErrorTracker } = require('./test_rtvi_client.js');

function expectRejected(tracker, label) {
  assert.throws(() => tracker.assertNoUnexpected(), /Unexpected RTVI error/, label);
}

const expected = createErrorTracker([]);
expected.onError({data:{error:'compat_test_error',fatal:false}});
expected.assertNoUnexpected();
assert.equal(expected.expectedSeen(), true);

const fatal = createErrorTracker([]);
fatal.onError({data:{error:'fatal_provider',fatal:true}});
expectRejected(fatal, 'fatal callback after connection must fail');

const late = createErrorTracker([]);
late.onError({data:{error:'compat_test_error',fatal:false}});
late.markDisconnected();
late.onError({data:{error:'late_failure',fatal:false}});
expectRejected(late, 'unexpected callback after disconnect must fail');

const duplicate = createErrorTracker([]);
duplicate.onError({data:{error:'compat_test_error',fatal:false}});
duplicate.onError({data:{error:'compat_test_error',fatal:false}});
expectRejected(duplicate, 'duplicate expected error must fail');
