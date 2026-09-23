import assert from 'node:assert/strict'
import test from 'node:test'
import { accountModelAvailability } from './accountModelAvailability.ts'

const now=1790132000
const entry=(outcome,transport='codex',at=now)=>({model:'gpt-6-sol',transport,outcome,observed_at:at,source:outcome==='listed'?'manifest':'probe'})
test('whitelist is not evidence and empty whitelist is unknown',()=>{
 assert.equal(accountModelAvailability({models:[]},'gpt-6-sol',now).state,'unknown')
 assert.equal(accountModelAvailability({models:['gpt-6-sol']},'gpt-6-sol',now).state,'configured')
 assert.equal(accountModelAvailability({models:['gpt-5.5'],model_observations:[entry('available')]},'gpt-6-sol',now).blocked,true)
})
test('BPS evidence is isolated and missing/stale evidence never means unsupported',()=>{
 assert.equal(accountModelAvailability({codex_bps_enabled:true,model_observations:[entry('available')]},'gpt-6-sol',now).state,'unknown')
 assert.equal(accountModelAvailability({codex_bps_enabled:true,model_observations:[entry('available','bps')]},'gpt-6-sol',now).state,'available')
 assert.equal(accountModelAvailability({model_observations:[entry('unsupported','codex',now-90000)]},'gpt-6-sol',now).state,'stale')
 for(const status of ['throttled','error','listed','unsupported']) assert.equal(accountModelAvailability({model_observations:[entry(status)]},'gpt-6-sol',now).state,status)
})
test('newest observation wins with probe preferred on ties',()=>{
 assert.equal(accountModelAvailability({model_observations:[entry('listed'),entry('unsupported')]},'GPT-6-SOL',now).state,'unsupported')
 assert.equal(accountModelAvailability({model_observations:[entry('unsupported'),entry('listed','codex',now+1)]},'gpt-6-sol',now+1).state,'listed')
})
