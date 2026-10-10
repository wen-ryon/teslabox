/** 限制固定递归解析器的对象图；重复引用按实际展开次数计数，拒绝循环和危险字典键。 */
export function validatePlistBudget(bytes) {
  if (!Buffer.isBuffer(bytes) || bytes.length < 40 || bytes.length > 131072
      || bytes.toString('ascii', 0, 8) !== 'bplist00') throw new Error('plist_budget_invalid');
  const trailer = bytes.length - 32;
  const offSize = bytes[trailer + 6], refSize = bytes[trailer + 7];
  const count = Number(bytes.readBigUInt64BE(trailer + 8));
  const top = Number(bytes.readBigUInt64BE(trailer + 16));
  const table = Number(bytes.readBigUInt64BE(trailer + 24));
  if (![1, 2, 4, 8].includes(offSize) || ![1, 2, 4, 8].includes(refSize)
      || !Number.isSafeInteger(count) || count < 1 || count > 4096 || !Number.isSafeInteger(top) || top >= count
      || !Number.isSafeInteger(table) || table < 8 || table + count * offSize !== trailer) throw new Error('plist_budget_invalid');
  const integer = (offset, length, limit) => {
    if (offset < 8 || offset + length > limit) throw new Error('plist_bounds');
    let value = 0;
    for (let i = 0; i < length; i++) value = value * 256 + bytes[offset + i];
    if (!Number.isSafeInteger(value)) throw new Error('plist_integer_budget');
    return value;
  };
  const offsets = Array.from({ length: count }, (_, i) => integer(table + i * offSize, offSize, trailer));
  let visits = 0, expandedBytes = 0;
  const path = new Set();
  function visit(index, depth, dictionaryKey = false) {
    if (index >= count || depth > 24 || ++visits > 8192 || path.has(index)) throw new Error('plist_graph_budget');
    path.add(index);
    let cursor = offsets[index];
    if (cursor < 8 || cursor >= table) throw new Error('plist_bounds');
    const marker = bytes[cursor++], type = marker >> 4, nibble = marker & 15;
    if (dictionaryKey && ![5, 6].includes(type)) throw new Error('plist_key_invalid');
    let length = nibble;
    if ([4, 5, 6, 10, 13].includes(type) && nibble === 15) {
      const lengthMarker = bytes[cursor++];
      if (lengthMarker >> 4 !== 1 || (lengthMarker & 15) > 3) throw new Error('plist_length_invalid');
      const width = 1 << (lengthMarker & 15);
      length = integer(cursor, width, table); cursor += width;
    }
    let payload = 0;
    if (type === 0) { if (![8, 9].includes(nibble)) throw new Error('plist_primitive_invalid'); }
    else if (type === 1 && nibble <= 3) payload = 1 << nibble;
    else if (type === 2 && [2, 3].includes(nibble)) payload = 1 << nibble;
    else if ([4, 5, 6].includes(type)) payload = length * (type === 6 ? 2 : 1);
    else if ([10, 13].includes(type)) payload = length * refSize * (type === 13 ? 2 : 1);
    else throw new Error('plist_type_invalid');
    if (cursor + payload > table || (expandedBytes += payload) > 262144) throw new Error('plist_expansion_budget');
    let key;
    if (dictionaryKey) {
      key = type === 5 ? bytes.toString('ascii', cursor, cursor + payload)
        : Buffer.from(bytes.subarray(cursor, cursor + payload)).swap16().toString('utf16le');
      if (['__proto__', 'prototype', 'constructor'].includes(key)) throw new Error('plist_key_invalid');
    }
    if (type === 10) for (let i = 0; i < length; i++) visit(integer(cursor + i * refSize, refSize, table), depth + 1);
    if (type === 13) {
      const keys = new Set();
      for (let i = 0; i < length; i++) {
        const name = visit(integer(cursor + i * refSize, refSize, table), depth + 1, true);
        if (keys.has(name)) throw new Error('plist_duplicate_key');
        keys.add(name);
        visit(integer(cursor + (length + i) * refSize, refSize, table), depth + 1);
      }
    }
    path.delete(index);
    return key;
  }
  visit(top, 0);
}
