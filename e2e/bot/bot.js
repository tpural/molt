// A player for molt's end-to-end test. It reads commands on stdin, one per
// line, and writes one JSON event per line on stdout.
//   setup   gives itself things, builds, and names a pig nearby
//   change  changes things again (run while the new server warms up)
//   frozen  places, breaks, runs a command and walks (run during the freeze)
//   check   says what it sees: position, inventory, blocks, the pig
import mineflayer from 'mineflayer'
import readline from 'node:readline'
import { Vec3 } from 'vec3'

const [host, port] = process.argv[2].split(':')
const say = (event, data = {}) => console.log(JSON.stringify({ event, ...data }))
const bot = mineflayer.createBot({ host, port: Number(port), username: 'molty', version: '26.1', auth: 'offline' })
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

let spawns = 0
bot.on('spawn', () => say('spawn', { n: ++spawns }))
// Packets a client gets when it's moved the usual way: a seamless switch has none.
let left = 0
bot._client.on('packet', (_, meta) => {
  if (spawns > 0 && ['start_configuration', 'login', 'respawn'].includes(meta.name)) {
    left++
    say('left', { why: meta.name })
  }
})
bot.on('end', (why) => { say('end', { why: String(why) }); process.exit(1) })
bot.on('kicked', (why) => say('kicked', { why: JSON.stringify(why) }))
bot.on('messagestr', (m) => say('chat', { why: m }))
bot.on('error', (err) => say('error', { why: String(err) }))

const at = (dx, dy, dz) => bot.entity.position.floored().offset(dx, dy, dz)
const where = (p) => `${p.x} ${p.y} ${p.z}`
let base, mark
async function run (cmd) {
  bot.chat(cmd)
  await sleep(300)
}

async function setup () {
  base = bot.entity.position.floored()
  await run('/gamemode creative')
  await run('/time set 6000')
  await run('/give molty diamond 5')
  await run(`/setblock ${where(base.offset(2, 0, 0))} gold_block`)
  await run(`/setblock ${where(base.offset(0, 0, 3))} chest{Items:[{Slot:0b,id:"minecraft:emerald",count:7}]}`)
  await run(`/summon pig ${where(base.offset(-3, 0, 0))} {CustomName:"Molty",NoAI:1b,PersistenceRequired:1b}`)
}

async function change () {
  await run('/give molty iron_ingot 3')
  await run(`/setblock ${where(base.offset(2, 1, 0))} diamond_block`)
  await run(`/setblock ${where(base.offset(2, 0, 0))} air`)
  // Walk a few blocks away.
  bot.setControlState('forward', true)
  await sleep(1000)
  bot.setControlState('forward', false)
  await sleep(500)
  // A block to break and build on while frozen, with room above it.
  mark = at(1, 0, 0)
  await run(`/fill ${where(mark.offset(0, 1, 0))} ${where(mark.offset(0, 3, 0))} air`)
  await run(`/setblock ${where(mark)} lapis_block`)
  await run('/give molty dirt 1')
  await sleep(500)
  await bot.equip(bot.registry.itemsByName.dirt.id, 'hand') // inventories don't change while frozen
}

async function frozen () {
  await bot.activateBlock(bot.blockAt(mark), new Vec3(0, 1, 0))
  bot.chat(`/setblock ${where(mark.offset(0, 2, 0))} emerald_block`)
  bot.dig(bot.blockAt(mark), 'ignore').catch(() => {}) // the frozen server may not say it's gone
  await sleep(200)
  bot.setControlState('back', true)
  await sleep(500)
  bot.setControlState('back', false)
  await sleep(300)
}

function count (name) {
  return bot.inventory.items().filter((i) => i.name === name).reduce((n, i) => n + i.count, 0)
}

function check () {
  const pigs = Object.values(bot.entities).filter((e) => e.name === 'pig' && JSON.stringify(e.metadata ?? '').includes('Molty'))
  const pig = pigs[0]
  const p = bot.entity.position
  return {
    pos: [p.x, p.y, p.z],
    diamond: count('diamond'),
    iron: count('iron_ingot'),
    gold: bot.blockAt(base.offset(2, 0, 0))?.name,
    above: bot.blockAt(base.offset(2, 1, 0))?.name,
    chest: bot.blockAt(base.offset(0, 0, 3))?.name,
    marks: mark && [0, 1, 2].map((dy) => bot.blockAt(mark.offset(0, dy, 0))?.name),
    pig: Boolean(pig),
    pigs: pigs.length,
    left,
    id: bot.entity.id,
    time: bot.time.timeOfDay
  }
}

bot.once('spawn', async () => {
  await bot.waitForChunksToLoad()
  say('ready')
  const rl = readline.createInterface({ input: process.stdin })
  for await (const line of rl) {
    try {
      if (line === 'setup') await setup()
      else if (line === 'change') await change()
      else if (line === 'frozen') await frozen()
      else if (line === 'check') { say('check', check()); continue }
      else if (line === 'quit') { bot.quit(); process.exit(0) }
      say('done', { cmd: line, ...check() })
    } catch (err) {
      say('error', { cmd: line, why: String(err) })
    }
  }
})
