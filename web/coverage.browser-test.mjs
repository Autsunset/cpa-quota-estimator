import {spawn} from 'node:child_process';
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';

const origin = process.argv[2];
const chromePath = process.argv[3];
const root = process.argv[4];
assert.equal(typeof WebSocket, 'function', 'Node.js 22 or newer is required');
const profile = await fs.mkdtemp(root + '/chrome-');
const chrome = spawn(chromePath, ['--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--no-default-browser-check', '--remote-debugging-port=0', '--user-data-dir=' + profile, 'about:blank'], {stdio: 'ignore'});
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
let socket;
try {
  let port;
  for (let i=0; i<100; i++) {
    try { port = (await fs.readFile(profile + '/DevToolsActivePort','utf8')).split('\n')[0]; break; } catch {}
    await pause(100);
  }
  assert(port, 'Chrome did not start');
  const pages = await (await fetch('http://127.0.0.1:' + port + '/json/list')).json();
  socket = new WebSocket(pages.find(page => page.type === 'page').webSocketDebuggerUrl);
  await new Promise(resolve => socket.addEventListener('open', resolve, {once:true}));
  let nextID = 0;
  const pending = new Map();
  const errors = [];
  socket.addEventListener('message', event => {
    const message = JSON.parse(event.data);
    if (message.method === 'Runtime.exceptionThrown') errors.push(message.params.exceptionDetails);
    if (message.id && pending.has(message.id)) {
      const {resolve,reject,timer} = pending.get(message.id);
      clearTimeout(timer);
      pending.delete(message.id);
      message.error ? reject(new Error(JSON.stringify(message.error))) : resolve(message.result);
    }
  });
  const send = (method,params={}) => new Promise((resolve,reject) => {
    const id=++nextID;
    const timer=setTimeout(()=>{pending.delete(id);reject(new Error('CDP request timed out: '+method));},5000);
    pending.set(id,{resolve,reject,timer});
    socket.send(JSON.stringify({id,method,params}));
  });
  const evaluate = async expression => {
    const result = await send('Runtime.evaluate',{expression,returnByValue:true,awaitPromise:true});
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description || result.exceptionDetails.text);
    return result.result.value;
  };
  const wait = async (expression,label) => {
    for (let i=0;i<150;i++) {
      if (await evaluate(expression)) return;
      await pause(100);
    }
    throw new Error('Timed out: ' + label + '\n' + await evaluate('document.body.innerText.slice(-1600)'));
  };
  const visible = id => evaluate(`!!document.getElementById(${JSON.stringify(id)})?.getClientRects().length`);
  const select = async (id,value) => evaluate(`document.getElementById(${JSON.stringify(id)}).value=${JSON.stringify(value)};document.getElementById(${JSON.stringify(id)}).dispatchEvent(new Event('change',{bubbles:true}));true`);
  const saveMode = async mode => {
    await select('coverageMode', mode);
    await evaluate("document.querySelector('#saveCoverageSettings').click();true");
    await wait(`state.collection_coverage.mode===${JSON.stringify(mode)} && !coverageSaving && $('#coverageSaveStatus').textContent.includes(language==='en'?'saved':'已保存')`, 'save ' + mode);
  };
  const screenshot = async name => {
    const result = await send('Page.captureScreenshot',{format:'png'});
    await fs.writeFile(root + '/' + name + '.png',Buffer.from(result.data,'base64'));
  };
  await send('Page.enable');
  await send('Runtime.enable');
  await send('Emulation.setDeviceMetricsOverride',{width:1365,height:1100,deviceScaleFactor:1,mobile:false});
  await send('Page.addScriptToEvaluateOnNewDocument',{source:"localStorage.setItem('cqe-persistent-key','local-browser-test');localStorage.setItem('cqe-language-mode','zh-CN');"});
  await send('Page.navigate',{url:origin});
  await wait("typeof state!=='undefined' && state?.account==='a-mixed' && seriesState && $('#fullTokens').textContent!=='—' && $('#monthTokens').textContent!=='—'", 'initial data');
  assert.equal(await evaluate("$('#coverageMode').value"),'cpa_only');
  assert((await evaluate("$('#coverageExplanation').textContent")).includes('假设全部用量经过 CPA'));
  assert.equal(await evaluate("document.querySelectorAll('input[name=pricingMode]').length"),3);
  assert.equal(await evaluate("document.querySelector('input[name=pricingMode]:checked').value"),'credits');
  assert.equal(await evaluate("$('#pricingTitle').textContent"),'计价口径');
  assert((await evaluate("$('#pricePreviewRows').children.length"))>=6,'price preview has model rows');
  assert.equal(await evaluate("document.getElementById('astraMultiplier')===null"),true);
  await evaluate("document.querySelector('input[name=pricingMode][value=custom]').click();true");
  assert.equal(await visible('customPriceEditor'),true);
  assert((await evaluate("$('#customPriceRows').children.length"))>=6,'custom editor has model rows');
  await evaluate("pricingSettingsDirty=false;document.querySelector('input[name=pricingMode][value=credits]').click();pricingSettingsDirty=false;renderPricingSettings(state.config,priceCatalogState);true");
  const originalTokens = await evaluate("$('#tokens').textContent");
  const originalQuota = await evaluate("$('#used').textContent");
  const originalCapacity = await evaluate("$('#fullTokens').textContent");
  assert(await visible('fullTokens'));
  await screenshot('coverage-default-desktop');
  await evaluate("$('#showSpark').checked=true;$('#showSpark').dispatchEvent(new Event('change',{bubbles:true}));true");
  await wait("seriesState?.spark_weekly_quota && document.getElementById('sparkWeeklyFullTokens')", 'Spark scopes');
  await saveMode('mixed');
  for (const id of ['fullTokens','capacityCurve','allowanceRows','weeklyFullTokens','weeklyAllowanceRows','sparkFullTokens','sparkWeeklyFullTokens','monthCapacity','weeklyMonthCapacity','sparkMonthCapacity','sparkWeeklyMonthCapacity']) {
    assert.equal(await visible(id),false, id + ' should be hidden in mixed mode');
  }
  for (const id of ['tokens','used','paceCurve','weeklyUsed','sparkUsed','sparkWeeklyUsed','monthTokens']) {
    assert.equal(await visible(id),true,id + ' observations should remain visible');
  }
  assert.equal(await evaluate("$('#tokens').textContent"),originalTokens);
  assert.equal(await evaluate("$('#used').textContent"),originalQuota);
  assert((await evaluate("$('#coverageSampleQuality').textContent")).includes('高'));
  assert.equal(await evaluate("seriesState.estimate.confidence"),'unavailable');
  await screenshot('coverage-mixed-desktop');
  await select('account','b-cpa');
  await wait("state.account==='b-cpa' && seriesState.account==='b-cpa' && !document.body.classList.contains('collection-limited')",'unaffected account');
  assert.equal(await evaluate("$('#coverageMode').value"),'cpa_only');
  assert(await visible('fullTokens'));
  const previousTimeOrigin = await evaluate('performance.timeOrigin');
  await send('Page.reload');
  await wait("typeof state!=='undefined' && performance.timeOrigin!=="+previousTimeOrigin+" && state?.account==='a-mixed' && seriesState && document.body.classList.contains('collection-limited')",'persisted mixed mode');
  assert.equal(await evaluate("$('#coverageMode').value"),'mixed');
  await saveMode('unknown');
  assert.equal(await evaluate('seriesState.estimate.unavailable_reason'),'usage_coverage_unknown');
  await send('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:1,mobile:true});
  await evaluate("$('#coverageTitle').scrollIntoView({block:'start'});true");
  assert(await evaluate("[...document.querySelectorAll('.collection-controls select,.collection-controls button')].every(el=>{let r=el.getBoundingClientRect();return r.left>=0 && r.right<=innerWidth;})"),'mobile controls overflow');
  await screenshot('coverage-unknown-mobile');
  await select('language','en');
  await wait("document.documentElement.lang==='en' && $('#coverageExplanation').textContent.startsWith('Coverage unknown')",'English coverage strings');
  assert.equal(await evaluate("$('#pricingTitle').textContent"),'Pricing basis');
  assert.equal(await evaluate("document.querySelector('.price-preview h2').textContent"),'Price table');
  assert.equal(await evaluate("document.querySelector('.pricing-choice small').textContent.startsWith('Start from current official API')"),true);
  assert.equal(await evaluate("$('#coverageSaveStatus').textContent"), 'Account collection coverage saved');
  await screenshot('coverage-unknown-mobile-en');
  await saveMode('cpa_only');
  assert.equal(await evaluate("$('#fullTokens').textContent"),originalCapacity);
  assert(await visible('fullTokens'));
  await fetch(origin + '/test/fail-refresh');
  await select('coverageMode','mixed');
  await evaluate("$('#saveCoverageSettings').click();true");
  await wait("!coverageSaving && $('#coverageSaveStatus').textContent.includes('data refresh failed')", 'saved setting with failed refresh');
  assert.equal(await evaluate('state.collection_coverage.mode'), 'mixed');
  assert.equal(await visible('fullTokens'),false);
  await evaluate("$('#resetRange').click();true");
  assert.equal(await visible('capacityCurve'),false,'cached chart must stay hidden after failed refresh');
  await evaluate("$('#refresh').click();true");
  await wait("seriesState.collection_coverage.mode==='mixed' && $('#coverageSaveStatus').textContent==='Account collection coverage saved'", 'refresh recovery');
  await saveMode('cpa_only');
  await fetch(origin + '/test/fail-write');
  await select('coverageMode','mixed');
  await evaluate("$('#saveCoverageSettings').click();true");
  await wait("!coverageSaving && $('#coverageSaveStatus').textContent.includes('injected save failure')",'failed save');
  assert.equal(await evaluate('state.collection_coverage.mode'),'cpa_only');
  assert(await visible('fullTokens'));
  await evaluate("$('#saveCoverageSettings').click();true");
  await wait("!coverageSaving && state.collection_coverage.mode==='mixed'",'retry save');
  await fetch(origin + '/test/delay-read');
  await evaluate("$('#refresh').click();true");
  await select('account','b-cpa');
  await wait("state.account==='b-cpa' && seriesState.account==='b-cpa'",'account switch during read');
  await pause(600);
  assert.equal(await evaluate('state.account'),'b-cpa');
  assert.equal(await evaluate("$('#coverageMode').value"),'cpa_only');
  await evaluate("$('#savePricingSettings').click();true");
  await wait("!$('#pricingTaskProgress').hidden",'background pricing task visible');
  await wait("$('#pricingTaskText').textContent.includes('Recalculation complete')",'background pricing task complete');
  assert.equal(await evaluate("$('#savePricingSettings').disabled"),false);
  assert.equal(errors.length,0,JSON.stringify(errors));
  console.log(JSON.stringify({defaultAssumption:true,mixedAndUnknown:true,independentScopes:true,persistence:true,accountIsolation:true,failedSaveRetry:true,failedRefreshProtection:true,staleReadProtection:true,mobile:true,english:true,pricingTask:true,javascriptErrors:errors.length}));
} finally {
  socket?.close();
  await new Promise(resolve => {
    if (chrome.exitCode !== null) return resolve();
    const timer=setTimeout(()=>chrome.kill('SIGKILL'),3000);
    chrome.once('exit',()=>{clearTimeout(timer);resolve();});
    chrome.kill('SIGTERM');
  });
}
