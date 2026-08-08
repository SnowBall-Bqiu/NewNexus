const messages = {
  overview: '总览', projects: '项目与站点', saas: 'Cloudflare SaaS', optimizer: 'CF 优选', routes: 'CNAME 线路',
  nodes: '探测节点', releases: '发布记录', alerts: '告警', connections: '厂商连接', settings: '系统设置',
};
export type MessageKey = keyof typeof messages;
export function t(key: MessageKey) { return messages[key]; }
