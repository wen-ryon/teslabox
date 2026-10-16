/* 无线地区独立保存，不把监管域写入 AP 密码或自动化配置。 */
(() => {
  "use strict";
  const t = globalThis.TeslaBoxI18n.t;
  const country = document.getElementById("wireless-region-country");
  const form = document.getElementById("wireless-region-form");
  const detail = document.getElementById("wireless-region-detail");
  const button = document.getElementById("wireless-region-save");
  if (!country || !form) return;
  let dirty = false;
  let busy = false;
  country.addEventListener("input", () => { dirty = true; });
  function render(value) {
    if (!dirty) country.value = value.configured_country || "";
    document.getElementById("wireless-region-configured").textContent = value.configured_country || t("未配置");
    document.getElementById("wireless-region-effective").textContent = value.effective_country || t("未知");
    document.getElementById("wireless-region-phys").textContent = (value.phy_domains || []).map(phy => `${phy.phy}：${phy.country || t("未知")}${phy.self_managed ? t("（网卡自管理）") : ""}`).join("；");
    const label = {applied:t("已核对生效"), restricted:t("部分网卡受限"), pending:t("已保存，尚未生效"), unavailable:t("无法核对")}[value.state] || t("状态未知");
    detail.textContent = [label, value.message, value.reason_code ? t`原因：${value.reason_code}` : "", value.last_error].filter(Boolean).join("；");
    // 生效仅指监管域，不代表 AP 或终端连接已验收；不隐藏自管理 PHY 的差异。
    detail.className = value.state === "applied" ? "muted" : "form-error";
    button.disabled = busy;
  }
  function unavailable(message) {
    document.getElementById("wireless-region-effective").textContent = t("未知");
    document.getElementById("wireless-region-phys").textContent = "";
    detail.textContent = t`无法读取无线地区：${message}；未确认实际生效状态。`;
    detail.className = "form-error";
  }
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (busy) return;
    const selected = country.value.trim().toUpperCase();
    if (!/^(?:[A-Z]{2}|00)$/.test(selected)) { unavailable(t("请输入实际使用地区的两位代码，如 CN")); return; }
    const confirmed = await askConfirmation(t("修改无线使用地区"), t`将地区设置为 ${selected}。这可能使当前 WiFi 或热点断开，CarPlay 运行期间不能修改。请确认设备确实在该地区使用，并已准备有线管理连接。`, t("确认保存并应用"), true);
    if (!confirmed) return;
    busy = true; button.disabled = true;
    try {
      const result = await postJSON("/api/v1/network/regulatory", {country:selected, confirm_network_change:true});
      dirty = false; state.regulatory = result; render(result);
      setSettingsNotice(result.state === "applied" ? t("无线地区已保存并核对生效，热点连接仍需单独验证") : t("无线地区已保存，但实际应用尚未完全确认，请查看地区状态"), result.state !== "applied");
    } catch (error) { unavailable(error.message); }
    finally { busy = false; button.disabled = false; }
  });
  globalThis.TeslaBoxWirelessRegion = {render, unavailable};
  if (state.regulatory) render(state.regulatory);
})();
