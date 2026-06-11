import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

describe('account status locale keys', () => {
  it('contains disabled status labels for imported invalid accounts', () => {
    expect(zh.admin.accounts.status.disabled).toBe('已禁用')
    expect(en.admin.accounts.status.disabled).toBe('Disabled')
  })
})
