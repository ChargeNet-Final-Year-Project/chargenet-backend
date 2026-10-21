"""Run the simulated MYR-wallet journey over real REST/OCPI.

Usage: python scripts/demo_roaming.py --base http://localhost:8080 --output demo.json
Requires a running ChargeNet backend; uses the seeded demo account and simulated
top-ups only. No database reset, commercial payment or external CPO is involved.
"""
import argparse
import json
import time
import uuid
from decimal import Decimal, ROUND_HALF_UP
from urllib.request import Request, urlopen


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--base', default='http://localhost:8080')
    parser.add_argument('--output', default='demo-roaming-results.json')
    args = parser.parse_args()
    token = None

    def call(method, path, body=None):
        headers = {'Content-Type': 'application/json'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        if method == 'POST':
            headers['Idempotency-Key'] = str(uuid.uuid4())
        request = Request(args.base + '/api/v1' + path,
                          None if body is None else json.dumps(body).encode(),
                          headers=headers, method=method)
        with urlopen(request, timeout=20) as response:
            return json.load(response)

    token = call('POST', '/auth/login', {'email': 'demo@chargenet.my', 'password': 'Demo1234!'})['token']
    wallet = call('GET', '/wallet')
    assert wallet['currency'] == 'MYR', 'Use a MYR demo wallet'
    if Decimal(str(wallet['availableBalance'])) < 50:
        call('POST', '/wallet/topup', {'amount': 100, 'paymentMethod': 'Credit Card'})
    records = []
    for country, operator in [('MY', 'cpo_voltgo'), ('SG', 'cpo_chargesync'), ('TH', 'cpo_ecowatt')]:
        stations = call('GET', '/stations?country=' + country + '&cpoId=' + operator)
        candidates = [(s, c) for s in stations if s['isSimulated']
                      for c in s['connectors'] if c['status'] == 'AVAILABLE' and c['powerType'] == 'DC']
        station, connector = candidates[0]
        before = call('GET', '/wallet')
        quote = call('POST', '/quotes', {'evseUid': connector['evseUid'], 'estimatedKwh': '15'})
        print(f"{country}: review {quote['originalCurrency']} {quote['tariffPerKwh']}/kWh; "
              f"MYR equivalent {quote['equivalentTariff']}; rate {quote['exchangeRate']}", flush=True)
        session = call('POST', '/sessions/start', {'evseUid': connector['evseUid'], 'quoteId': quote['id'], 'targetSoc': 100})
        deadline = time.monotonic() + 25
        while time.monotonic() < deadline:
            time.sleep(.5)
            session = call('GET', '/sessions/' + session['id'])
            if session['kwhDelivered'] > 0:
                break
        assert session['kwhDelivered'] > 0, 'No live telemetry'
        live = session
        receipt = call('POST', '/sessions/' + session['id'] + '/stop')
        while receipt['status'] in ('PENDING', 'CHARGING') and time.monotonic() < deadline:
            time.sleep(.2)
            receipt = call('GET', '/sessions/' + session['id'])
        assert receipt['status'] == 'COMPLETED' and receipt['cdrId']
        after = call('GET', '/wallet')
        original = (Decimal(str(receipt['kwhDelivered'])) * Decimal(quote['tariffPerKwh'])).quantize(Decimal('.01'), ROUND_HALF_UP)
        deduction = (original * Decimal(quote['exchangeRate'])).quantize(Decimal('.01'), ROUND_HALF_UP)
        assert Decimal(str(receipt['originalCost'])) == original
        assert Decimal(str(receipt['totalCost'])) == deduction
        assert Decimal(str(before['balance'])) - Decimal(str(after['balance'])) == deduction
        assert after['reserved'] == 0
        assert receipt['cdrJson']['currency'] == quote['originalCurrency']
        transactions = call('GET', '/wallet/transactions')
        linked = [t for t in transactions if t.get('sessionId') == receipt['id']]
        assert len(linked) == 1 and linked[0]['cdrId'] == receipt['cdrId']
        repeated = call('POST', '/sessions/' + session['id'] + '/stop')
        assert repeated['totalCost'] == receipt['totalCost']
        assert call('GET', '/wallet')['balance'] == after['balance']
        records.append(dict(country=country, station=station, connector=connector,
                            quote=quote, live=live, receipt=receipt,
                            walletBefore=before, walletAfter=after, transaction=linked[0]))
        print(f"  {receipt['kwhDelivered']} kWh; {quote['originalCurrency']} {original}; "
              f"MYR {deduction} deducted; CDR {receipt['cdrId']}", flush=True)
    with open(args.output, 'w', encoding='utf8') as output:
        json.dump(records, output, indent=2)
    print('Verified three countries, wallet deductions, CDRs and exactly-once stop.')


if __name__ == '__main__':
    main()
