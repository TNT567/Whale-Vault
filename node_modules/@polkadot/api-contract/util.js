export function applyOnEvent(result, types, fn, isRevive) {
    if (result.isInBlock || result.isFinalized) {
        const section = isRevive ? 'revive' : 'contracts';
        const records = result.filterRecords(section, types);
        if (records.length) {
            return fn(records);
        }
    }
    return undefined;
}
