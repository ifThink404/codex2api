import assert from 'node:assert/strict'
import test from 'node:test'

import { getUsageBillingTier } from './usageBillingTier.ts'

test('unsolicited upstream priority does not become a billed Fast badge', () => {
  assert.equal(getUsageBillingTier({ service_tier: 'fast', actual_service_tier: 'priority', billing_service_tier: '' }), 'default')
  assert.equal(getUsageBillingTier({ service_tier: 'fast', requested_service_tier: 'default', actual_service_tier: 'priority' }), 'default')
})

test('explicit billing decision and legacy-only tiers remain visible', () => {
  assert.equal(getUsageBillingTier({ service_tier: 'fast', actual_service_tier: 'priority', billing_service_tier: 'default' }), 'default')
  assert.equal(getUsageBillingTier({ service_tier: 'default', actual_service_tier: 'default', billing_service_tier: 'priority' }), 'priority')
  assert.equal(getUsageBillingTier({ service_tier: 'fast' }), 'fast')
  assert.equal(getUsageBillingTier({}), '')
})
