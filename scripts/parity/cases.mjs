// Run every parity case against one target and write the results as JSON, with the run id replaced by <run>.
// The file is rewritten after each case, so it keeps the finished cases if the target dies.
// Usage: node cases.mjs <out.json> <project> <instance> <table> <aggregate family> <plain family>
// The aggregate family must be an int64 MIN, MAX, or Sum aggregate.
// Each case name starts with the aggregate family's name.
import { writeFileSync } from 'node:fs'
import { caseKey, connect } from './client.mjs'

const [out, projectId, instanceId, tableId, AGG, PLAIN] = process.argv.slice(2)
const runId = crypto.randomUUID()
const { client, tableName, table } = connect(projectId, instanceId, tableId)

const TS = 1000
const COL = 'c'
const utf8 = (s) => Buffer.from(s, 'utf8')
const be = (n) => {
    const b = Buffer.alloc(8)
    b.writeBigInt64BE(BigInt(n))
    return b
}

const setCell = (family, { column = COL, value = be(456), ts = TS } = {}) => ({
    setCell: { familyName: family, columnQualifier: utf8(column), value, timestampMicros: ts },
})
const addToCell = (family, { column = COL, input = { intValue: 456 }, ts = TS } = {}) => ({
    addToCell: { familyName: family, columnQualifier: { rawValue: utf8(column) }, input, timestamp: { rawTimestampMicros: ts } },
})
const mergeToCell = (family, { column = COL, input = { bytesValue: be(456) }, ts = TS } = {}) => ({
    mergeToCell: { familyName: family, columnQualifier: { rawValue: utf8(column) }, input, timestamp: { rawTimestampMicros: ts } },
})
const withoutInput = (mutation) => {
    for (const m of Object.values(mutation)) delete m.input
    return mutation
}
const deleteFromColumn = (family, timeRange) => ({ deleteFromColumn: { familyName: family, columnQualifier: utf8(COL), timeRange } })
const deleteFromFamily = (family) => ({ deleteFromFamily: { familyName: family } })
const deleteFromRow = () => ({ deleteFromRow: {} })
const increment = (family) => ({ familyName: family, columnQualifier: utf8(COL), incrementAmount: 1 })
const append = (family) => ({ familyName: family, columnQualifier: utf8(COL), appendValue: utf8('x') })

const key = (name) => caseKey(runId, name)
const errorOf = (err) => ({ code: err.code, details: String(err.details ?? err.message) })

const attempt = async (fn) => {
    try {
        return (await fn()) ?? 'ok'
    } catch (err) {
        return errorOf(err)
    }
}

const mutateRow = (name, mutations) => attempt(() => client.mutateRow({ tableName, rowKey: utf8(key(name)), mutations }).then(() => {}))

const mutateRows = (entries) =>
    attempt(async () => {
        const statuses = []
        const stream = client.mutateRows({ tableName, entries: entries.map(([name, mutations]) => ({ rowKey: utf8(key(name)), mutations })) })
        for await (const response of stream) {
            for (const entry of response.entries) {
                statuses[Number(entry.index)] = { code: entry.status.code, message: entry.status.message }
            }
        }
        return { statuses }
    })

const checkAndMutateRow = (name, branches) =>
    attempt(async () => {
        const [response] = await client.checkAndMutateRow({ tableName, rowKey: utf8(key(name)), ...branches })
        return { predicateMatched: response.predicateMatched }
    })

const readModifyWriteRow = (name, rules) => attempt(() => client.readModifyWriteRow({ tableName, rowKey: utf8(key(name)), rules }).then(() => {}))

// The stored cells as raw bytes. The Node client's row reads decode 8-byte values into numbers.
const rawRead = async (name) => {
    const out = {}
    let family, column, cell
    for await (const response of client.readRows({ tableName, rows: { rowKeys: [utf8(key(name))] } })) {
        for (const chunk of response.chunks) {
            if (chunk.familyName) family = chunk.familyName.value
            if (chunk.qualifier) column = Buffer.from(chunk.qualifier.value).toString('utf8')
            if (chunk.familyName || chunk.qualifier || chunk.timestampMicros != null) {
                cell = { ts: String(chunk.timestampMicros), hex: '' }
                ;(out[`${family}:${column}`] ??= []).push(cell)
            }
            cell.hex += Buffer.from(chunk.value ?? []).toString('hex')
        }
    }
    return Object.keys(out).length ? out : null
}

// A read of the latest cell in each column, keeping the client's decoded data.
const latestRead = async (name) => {
    try {
        const [row] = await table.row(key(name)).get({ filter: { column: { cellLimit: 1 } } })
        return row.data
    } catch (err) {
        if (err instanceof Error && err.message.includes('Unknown row:')) return null
        return errorOf(err)
    }
}

// Write each step's mutations to one row, reading the row after each.
const steps = async (name, list) => {
    const out = []
    for (const [label, mutations] of list) {
        const write = await mutateRow(name, mutations)
        out.push({ step: label, write, read: await latestRead(name), raw: await rawRead(name) })
    }
    return out
}

// Seed the row, apply one call, then read the row.
const afterSeed = async (name, seed, call) => {
    if (seed.length) await client.mutateRow({ tableName, rowKey: utf8(key(name)), mutations: seed })
    return { result: await call(name), row: await rawRead(name) }
}

const cases = {}
const run = async (name, fn) => {
    const caseName = `${AGG}: ${name}`
    try {
        cases[caseName] = await fn()
    } catch (err) {
        cases[caseName] = { caseError: errorOf(err) }
    }
    writeFileSync(out, JSON.stringify(cases, null, 2).replaceAll(runId, '<run>'))
}

const seedAgg = [addToCell(AGG)]
const seedPlain = [setCell(PLAIN)]
const seedBoth = [addToCell(AGG), setCell(PLAIN)]

// An application's write and read paths.
await run('AddToCell merges at one timestamp', () =>
    steps('one-ts-add', [
        ['AddToCell 456', [addToCell(AGG)]],
        ['AddToCell 123', [addToCell(AGG, { input: { intValue: 123 } })]],
        ['AddToCell 789', [addToCell(AGG, { input: { intValue: 789 } })]],
    ]),
)
await run('row write and latest-cell read', () =>
    steps('row', [
        [
            'replace row',
            [
                setCell(PLAIN, { column: 'version', value: be(1) }),
                addToCell(AGG),
                setCell(PLAIN, { column: 'updatedAt', value: be(123), ts: 123000 }),
                setCell(PLAIN, { column: 'expiresAt', value: be(789), ts: 123000 }),
                setCell(PLAIN, { column: 'flag', value: be(1), ts: 789000 }),
                setCell(PLAIN, { column: 'labels', value: utf8('["default"]'), ts: 123000 }),
                setCell(PLAIN, { column: 'createdAt', value: be(222), ts: 222000 }),
            ],
        ],
        ['replace row again', [setCell(PLAIN, { column: 'updatedAt', value: be(456), ts: 456000 }), addToCell(AGG, { input: { intValue: 678 } })]],
    ]),
)
await run('Node table.insert on the aggregate family', async () => {
    const write = await attempt(() => table.insert({ key: key('insert'), data: { [AGG]: { [COL]: { value: 456, timestamp: new Date(1) } } } }).then(() => {}))
    return { write, read: await latestRead('insert'), raw: await rawRead('insert') }
})
await run('DeleteFromColumn on the aggregate family, then AddToCell', () =>
    steps('delete-then-add', [
        ['AddToCell 456', [addToCell(AGG)]],
        ['DeleteFromColumn', [deleteFromColumn(AGG)]],
        ['AddToCell 678', [addToCell(AGG, { input: { intValue: 678 } })]],
    ]),
)
await run('AddToCell at two timestamps', () =>
    steps('two-ts', [
        ['456 at 1000', [addToCell(AGG)]],
        ['123 at 2000', [addToCell(AGG, { input: { intValue: 123 }, ts: 2000 })]],
    ]),
)
await run('MergeToCell merges at one timestamp', () =>
    steps('one-ts-merge', [
        ['MergeToCell 456', [mergeToCell(AGG)]],
        ['MergeToCell 123', [mergeToCell(AGG, { input: { bytesValue: be(123) } })]],
        ['MergeToCell 789', [mergeToCell(AGG, { input: { bytesValue: be(789) } })]],
    ]),
)
await run('AddToCell compares negative values as signed', () =>
    steps('signed-add', [
        ['AddToCell -3', [addToCell(AGG, { input: { intValue: -3 } })]],
        ['AddToCell 5', [addToCell(AGG, { input: { intValue: 5 } })]],
    ]),
)
await run('MergeToCell compares negative values as signed', () =>
    steps('signed-merge', [
        ['MergeToCell -3', [mergeToCell(AGG, { input: { bytesValue: be(-3) } })]],
        ['MergeToCell 5', [mergeToCell(AGG, { input: { bytesValue: be(5) } })]],
    ]),
)

// Which mutations MutateRow accepts on each family type.
const mutateRowCase = (name, seed, mutations) => afterSeed(name, seed, (n) => mutateRow(n, mutations))
await run('MutateRow SetCell on aggregate', () => mutateRowCase('set-agg', [], [setCell(AGG)]))
await run('MutateRow SetCell on plain', () => mutateRowCase('set-plain', [], [setCell(PLAIN)]))
await run('MutateRow AddToCell on aggregate', () => mutateRowCase('add-agg', [], [addToCell(AGG)]))
await run('MutateRow AddToCell on plain', () => mutateRowCase('add-plain', [], [addToCell(PLAIN)]))
await run('MutateRow MergeToCell on aggregate', () => mutateRowCase('merge-agg', [], [mergeToCell(AGG)]))
await run('MutateRow MergeToCell on plain', () => mutateRowCase('merge-plain', [], [mergeToCell(PLAIN)]))
await run('MutateRow DeleteFromColumn on aggregate', () => mutateRowCase('delcol-agg', seedAgg, [deleteFromColumn(AGG)]))
await run('MutateRow DeleteFromColumn with a time range on aggregate', () =>
    mutateRowCase('delcol-range-agg', seedAgg, [deleteFromColumn(AGG, { startTimestampMicros: TS, endTimestampMicros: TS + 1000 })]),
)
await run('MutateRow DeleteFromFamily on aggregate', () => mutateRowCase('delfam-agg', seedBoth, [deleteFromFamily(AGG)]))
await run('MutateRow DeleteFromRow with aggregate cells', () => mutateRowCase('delrow', seedBoth, [deleteFromRow()]))
await run('MutateRow AddToCell then SetCell on aggregate', () => mutateRowCase('atomic-agg', [], [addToCell(AGG), setCell(AGG)]))
await run('MutateRow SetCell on plain then SetCell on aggregate', () => mutateRowCase('atomic-plain', [], [setCell(PLAIN), setCell(AGG)]))

// MutateRows batches.
const mutateRowsCase = async (entries) => {
    const result = await mutateRows(entries)
    const rows = {}
    for (const [name] of entries) rows[name] = await rawRead(name)
    return { result, rows }
}
await run('MutateRows valid entries', () => mutateRowsCase([['rows-g1', [addToCell(AGG)]], ['rows-g2', [setCell(PLAIN)]]]))
await run('MutateRows SetCell on aggregate, then a valid entry', () => mutateRowsCase([['rows-b1', [setCell(AGG)]], ['rows-g3', [addToCell(AGG)]]]))
await run('MutateRows a valid entry, then SetCell on aggregate', () => mutateRowsCase([['rows-g4', [setCell(PLAIN)]], ['rows-b2', [setCell(AGG)]]]))
await run('MutateRows AddToCell on plain, then a valid entry', () => mutateRowsCase([['rows-b3', [addToCell(PLAIN)]], ['rows-g5', [setCell(PLAIN)]]]))

// CheckAndMutateRow applies one branch. With no predicate, a row with cells takes the true branch.
await run('CheckAndMutateRow SetCell on aggregate in the applied branch', () =>
    afterSeed('cam-agg', seedPlain, (n) => checkAndMutateRow(n, { trueMutations: [setCell(AGG)] })),
)
await run('CheckAndMutateRow SetCell on aggregate in the branch it skips', () =>
    afterSeed('cam-skipped', seedPlain, (n) => checkAndMutateRow(n, { trueMutations: [addToCell(AGG)], falseMutations: [setCell(AGG)] })),
)

// ReadModifyWriteRow.
for (const [label, rule] of [
    ['increment', increment],
    ['append', append],
]) {
    await run(`ReadModifyWriteRow ${label} on aggregate`, () => afterSeed(`rmw-${label}-agg`, seedAgg, (n) => readModifyWriteRow(n, [rule(AGG)])))
    await run(`ReadModifyWriteRow ${label} on empty aggregate`, () => afterSeed(`rmw-${label}-empty`, [], (n) => readModifyWriteRow(n, [rule(AGG)])))
}
await run('ReadModifyWriteRow increment on plain, then on aggregate', () =>
    afterSeed('rmw-atomic', seedPlain, (n) => readModifyWriteRow(n, [increment(PLAIN), increment(AGG)])),
)

// Input kinds, checked for the whole request.
const intAsBytes = { input: { bytesValue: be(456) } }
const bytesAsInt = { input: { intValue: 456 } }
await run('MutateRow AddToCell with a bytes input', () => mutateRowCase('add-bytes', [], [addToCell(AGG, intAsBytes)]))
await run('MutateRow MergeToCell with an int input', () => mutateRowCase('merge-int', [], [mergeToCell(AGG, bytesAsInt)]))
await run('MutateRow MergeToCell with a raw input', () => mutateRowCase('merge-raw', [], [mergeToCell(AGG, { input: { rawValue: be(456) } })]))
await run('MutateRow SetCell on plain, then AddToCell with a bytes input', () =>
    mutateRowCase('input-second', [], [setCell(PLAIN), addToCell(AGG, intAsBytes)]),
)
await run('MutateRows a valid entry, then AddToCell with a bytes input', () =>
    mutateRowsCase([['rows-input-good', [setCell(PLAIN)]], ['rows-input-bad', [addToCell(AGG, intAsBytes)]]]),
)
await run('CheckAndMutateRow AddToCell with a bytes input in the applied branch', () =>
    afterSeed('cam-input-applied', seedPlain, (n) => checkAndMutateRow(n, { trueMutations: [setCell(PLAIN), addToCell(AGG, intAsBytes)] })),
)
await run('CheckAndMutateRow AddToCell with a bytes input in the branch it skips', () =>
    afterSeed('cam-input-skipped', seedPlain, (n) => checkAndMutateRow(n, { trueMutations: [setCell(PLAIN)], falseMutations: [addToCell(AGG, intAsBytes)] })),
)

// The cases on an empty cell run first, so a crash on a merge into 456 leaves their results.
const threeBytes = be(456).subarray(5)
const oddLengths = [
    ['a 0-byte', Buffer.alloc(0)],
    ['a 3-byte', threeBytes],
    ['a 9-byte', Buffer.concat([Buffer.alloc(1), be(456)])],
]
for (const [label, bytesValue] of oddLengths) {
    await run(`MergeToCell with ${label} input on an empty cell`, () => mutateRowCase(`merge-${bytesValue.length}-empty`, [], [mergeToCell(AGG, { input: { bytesValue } })]))
}
const mergeThreeBytes = mergeToCell(AGG, { input: { bytesValue: threeBytes } })
await run('MutateRow SetCell on plain, then MergeToCell with a 3-byte input', () => mutateRowCase('merge-3-second', [], [setCell(PLAIN), mergeThreeBytes]))
await run('MutateRows MergeToCell with a 3-byte input, then a valid entry', () =>
    mutateRowsCase([['rows-merge-3-bad', [mergeThreeBytes]], ['rows-merge-3-good', [setCell(PLAIN)]]]),
)
await run('CheckAndMutateRow MergeToCell with a 3-byte input in the applied branch', () =>
    afterSeed('cam-merge-3-applied', seedPlain, (n) => checkAndMutateRow(n, { trueMutations: [setCell(PLAIN), mergeThreeBytes] })),
)
await run('CheckAndMutateRow MergeToCell with a 3-byte input in the branch it skips', () =>
    afterSeed('cam-merge-3-skipped', seedPlain, (n) => checkAndMutateRow(n, { trueMutations: [setCell(PLAIN)], falseMutations: [mergeThreeBytes] })),
)
for (const [label, bytesValue] of oddLengths) {
    await run(`MergeToCell with ${label} input on 456`, () => mutateRowCase(`merge-${bytesValue.length}-seeded`, seedAgg, [mergeToCell(AGG, { input: { bytesValue } })]))
}

// NULL inputs: an input with no kind, or no input at all. They crash Google's stock emulator, so they run last.
for (const [label, write] of [
    ['AddToCell', addToCell],
    ['MergeToCell', mergeToCell],
]) {
    await run(`${label} with no input on an empty cell`, () => mutateRowCase(`${label}-none-empty`, [], [withoutInput(write(AGG))]))
    await run(`${label} with an empty input on an empty cell`, () => mutateRowCase(`${label}-empty-empty`, [], [write(AGG, { input: {} })]))
    await run(`${label} with no input on 456`, () => mutateRowCase(`${label}-none-seeded`, seedAgg, [withoutInput(write(AGG))]))
}
