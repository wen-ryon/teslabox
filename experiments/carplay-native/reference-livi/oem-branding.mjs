import { readFileSync } from 'node:fs';

// 手机只在存在 OEM 图标时采用名称，因此两者必须一起提供。
export function teslaBoxBranding() {
  return {
    oemLabel: 'TeslaBox',
    icons: [120, 180, 256].map(size => ({
      widthPixels: size,
      heightPixels: size,
      data: readFileSync(new URL(`./assets/teslabox-${size}.png`, import.meta.url))
    }))
  };
}
